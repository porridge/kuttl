package test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"

	harness "github.com/kudobuilder/kuttl/pkg/apis/testharness/v1beta1"
	"github.com/kudobuilder/kuttl/pkg/env"
	kfile "github.com/kudobuilder/kuttl/pkg/file"
	"github.com/kudobuilder/kuttl/pkg/http"
	testutils "github.com/kudobuilder/kuttl/pkg/test/utils"
)

// fileNameRegex contains two capturing groups to determine whether a file has special
// meaning (ex. assert) or contains an appliable object, and extra name elements.
var fileNameRegex = regexp.MustCompile(`^(?:\d+-)?([^-\.]+)(-[^\.]+)?(?:\.gotmpl)?(?:\.yaml)?$`)

// A Step contains the name of the test step, its index in the test,
// and all of the test step's settings (including objects to apply and assert on).
type Step struct {
	Name  string
	Index int

	Dir string

	Step   *harness.TestStep
	Assert *harness.TestAssert

	Asserts []client.Object
	Apply   []client.Object
	Errors  []client.Object

	Timeout int

	Kubeconfig      string
	Client          func(forceNew bool) (client.Client, error)
	DiscoveryClient func() (discovery.DiscoveryInterface, error)

	Logger testutils.Logger
}

// Clean deletes all resources defined in the Apply list.
func (s *Step) Clean(namespace string) error {
	cl, err := s.Client(false)
	if err != nil {
		return err
	}

	dClient, err := s.DiscoveryClient()
	if err != nil {
		return err
	}

	for _, obj := range s.Apply {
		_, _, err := testutils.Namespaced(dClient, obj, namespace)
		if err != nil {
			return err
		}

		if err := cl.Delete(context.TODO(), obj); err != nil && !k8serrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// DeleteExisting deletes any resources in the TestStep.Delete list prior to running the tests.
func (s *Step) DeleteExisting(namespace string) error {
	cl, err := s.Client(false)
	if err != nil {
		return err
	}

	dClient, err := s.DiscoveryClient()
	if err != nil {
		return err
	}

	toDelete := []client.Object{}

	if s.Step == nil {
		return nil
	}

	for _, ref := range s.Step.Delete {
		gvk := ref.GroupVersionKind()

		obj := testutils.NewResource(gvk.GroupVersion().String(), gvk.Kind, ref.Name, "")

		objNs := namespace
		if ref.Namespace != "" {
			objNs = ref.Namespace
		}

		_, objNs, err := testutils.Namespaced(dClient, obj, objNs)
		if err != nil {
			return err
		}

		if ref.Name == "" {
			u := &unstructured.UnstructuredList{}
			u.SetGroupVersionKind(gvk)

			listOptions := []client.ListOption{}

			if ref.Labels != nil {
				listOptions = append(listOptions, client.MatchingLabels(ref.Labels))
			}

			if objNs != "" {
				listOptions = append(listOptions, client.InNamespace(objNs))
			}

			err := cl.List(context.TODO(), u, listOptions...)
			if err != nil {
				return fmt.Errorf("listing matching resources: %w", err)
			}

			for index := range u.Items {
				toDelete = append(toDelete, &u.Items[index])
			}
		} else {
			// Otherwise just append the object specified.
			toDelete = append(toDelete, obj.DeepCopy())
		}
	}

	for _, obj := range toDelete {
		delete := &unstructured.Unstructured{}
		delete.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		delete.SetName(obj.GetName())
		delete.SetNamespace(obj.GetNamespace())

		err := cl.Delete(context.TODO(), delete)
		if err != nil && !k8serrors.IsNotFound(err) {
			return err
		}
	}

	// Wait for resources to be deleted.
	return wait.PollImmediate(100*time.Millisecond, time.Duration(s.GetTimeout())*time.Second, func() (done bool, err error) {
		for _, obj := range toDelete {
			actual := &unstructured.Unstructured{}
			actual.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
			err = cl.Get(context.TODO(), testutils.ObjectKey(obj), actual)
			if err == nil || !k8serrors.IsNotFound(err) {
				return false, err
			}
		}

		return true, nil
	})
}

// Create applies all resources defined in the Apply list.
func (s *Step) Create(namespace string) []error {
	cl, err := s.Client(true)
	if err != nil {
		return []error{err}
	}

	dClient, err := s.DiscoveryClient()
	if err != nil {
		return []error{err}
	}

	errors := []error{}

	for _, obj := range s.Apply {
		_, _, err := testutils.Namespaced(dClient, obj, namespace)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		ctx := context.Background()
		if s.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(s.Timeout)*time.Second)
			defer cancel()
		}

		if updated, err := testutils.CreateOrUpdate(ctx, cl, obj, true); err != nil {
			errors = append(errors, err)
		} else {
			action := "created"
			if updated {
				action = "updated"
			}
			s.Logger.Log(testutils.ResourceID(obj), action)
		}
	}

	return errors
}

// GetTimeout gets the timeout defined for the test step.
func (s *Step) GetTimeout() int {
	timeout := s.Timeout
	if s.Assert != nil && s.Assert.Timeout != 0 {
		timeout = s.Assert.Timeout
	}
	return timeout
}

func list(cl client.Client, gvk schema.GroupVersionKind, namespace string) ([]unstructured.Unstructured, error) {
	list := unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)

	listOptions := []client.ListOption{}
	if namespace != "" {
		listOptions = append(listOptions, client.InNamespace(namespace))
	}

	if err := cl.List(context.TODO(), &list, listOptions...); err != nil {
		return []unstructured.Unstructured{}, err
	}

	return list.Items, nil
}

// CheckResource checks if the expected resource's state in Kubernetes is correct.
func (s *Step) CheckResource(expected runtime.Object, namespace string) []error {
	cl, err := s.Client(false)
	if err != nil {
		return []error{err}
	}

	dClient, err := s.DiscoveryClient()
	if err != nil {
		return []error{err}
	}

	testErrors := []error{}

	name, namespace, err := testutils.Namespaced(dClient, expected, namespace)
	if err != nil {
		return append(testErrors, err)
	}

	gvk := expected.GetObjectKind().GroupVersionKind()

	actuals := []unstructured.Unstructured{}

	if name != "" {
		actual := unstructured.Unstructured{}
		actual.SetGroupVersionKind(gvk)

		err = cl.Get(context.TODO(), client.ObjectKey{
			Namespace: namespace,
			Name:      name,
		}, &actual)

		actuals = append(actuals, actual)
	} else {
		actuals, err = list(cl, gvk, namespace)
		if len(actuals) == 0 {
			testErrors = append(testErrors, fmt.Errorf("no resources matched of kind: %s", gvk.String()))
		}
	}
	if err != nil {
		return append(testErrors, err)
	}

	expectedObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(expected)
	if err != nil {
		return append(testErrors, err)
	}

	for _, actual := range actuals {
		actual := actual

		tmpTestErrors := []error{}

		if err := testutils.IsSubset(expectedObj, actual.UnstructuredContent()); err != nil {
			diff, diffErr := testutils.PrettyDiff(expected, &actual)
			if diffErr == nil {
				tmpTestErrors = append(tmpTestErrors, fmt.Errorf(diff))
			} else {
				tmpTestErrors = append(tmpTestErrors, diffErr)
			}

			tmpTestErrors = append(tmpTestErrors, fmt.Errorf("resource %s: %s", testutils.ResourceID(expected), err))
		}

		if len(tmpTestErrors) == 0 {
			return tmpTestErrors
		}

		testErrors = append(testErrors, tmpTestErrors...)
	}

	return testErrors
}

// CheckResourceAbsent checks if the expected resource's state is absent in Kubernetes.
func (s *Step) CheckResourceAbsent(expected runtime.Object, namespace string) error {
	cl, err := s.Client(false)
	if err != nil {
		return err
	}

	dClient, err := s.DiscoveryClient()
	if err != nil {
		return err
	}

	name, namespace, err := testutils.Namespaced(dClient, expected, namespace)
	if err != nil {
		return err
	}

	gvk := expected.GetObjectKind().GroupVersionKind()

	var actuals []unstructured.Unstructured

	if name != "" {
		actual := unstructured.Unstructured{}
		actual.SetGroupVersionKind(gvk)

		if err := cl.Get(context.TODO(), client.ObjectKey{
			Namespace: namespace,
			Name:      name,
		}, &actual); err != nil {
			if k8serrors.IsNotFound(err) {
				return nil
			}

			return err
		}

		actuals = []unstructured.Unstructured{actual}
	} else {
		actuals, err = list(cl, gvk, namespace)
		if err != nil {
			return err
		}
	}

	expectedObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(expected)
	if err != nil {
		return err
	}

	for _, actual := range actuals {
		if err := testutils.IsSubset(expectedObj, actual.UnstructuredContent()); err == nil {
			return fmt.Errorf("resource matched of kind: %s", gvk.String())
		}
	}

	return nil
}

// CheckAssertCommands Runs the commands provided in `commands` and check if have been run successfully.
// the errors returned can be a a failure of executing the command or the failure of the command executed.
func (s *Step) CheckAssertCommands(ctx context.Context, namespace string, commands []harness.TestAssertCommand, timeout int) []error {
	testErrors := []error{}
	if _, err := testutils.RunAssertCommands(ctx, s.Logger, namespace, commands, "", timeout, s.Kubeconfig); err != nil {
		testErrors = append(testErrors, err)
	}
	return testErrors
}

// Check checks if the resources defined in Asserts and Errors are in the correct state.
func (s *Step) Check(namespace string, timeout int) []error {
	testErrors := []error{}

	for _, expected := range s.Asserts {
		testErrors = append(testErrors, s.CheckResource(expected, namespace)...)
	}

	if s.Assert != nil {
		testErrors = append(testErrors, s.CheckAssertCommands(context.TODO(), namespace, s.Assert.Commands, timeout)...)
	}

	for _, expected := range s.Errors {
		if testError := s.CheckResourceAbsent(expected, namespace); testError != nil {
			testErrors = append(testErrors, testError)
		}
	}

	return testErrors
}

// Run runs a KUTTL test step:
// 1. Apply all desired objects to Kubernetes.
// 2. Wait for all of the states defined in the test step's asserts to be true.'
func (s *Step) Run(namespace string) []error {
	s.Logger.Log("starting test step", s.String())

	if err := s.DeleteExisting(namespace); err != nil {
		return []error{err}
	}

	testErrors := []error{}

	if s.Step != nil {
		for _, command := range s.Step.Commands {
			if command.Background {
				s.Logger.Log("background commands are not allowed for steps and will be run in foreground")
				command.Background = false
			}
		}
		if _, err := testutils.RunCommands(context.TODO(), s.Logger, namespace, s.Step.Commands, s.Dir, s.Timeout, s.Kubeconfig); err != nil {
			testErrors = append(testErrors, err)
		}
	}

	testErrors = append(testErrors, s.Create(namespace)...)

	if len(testErrors) != 0 {
		return testErrors
	}

	timeoutF := float64(s.GetTimeout())
	start := time.Now()

	for elapsed := 0.0; elapsed < timeoutF; elapsed = time.Since(start).Seconds() {
		testErrors = s.Check(namespace, int(timeoutF-elapsed))

		if len(testErrors) == 0 {
			break
		}
		if hasTimeoutErr(testErrors) {
			break
		}
		time.Sleep(time.Second)
	}

	// all is good
	if len(testErrors) == 0 {
		s.Logger.Log("test step completed", s.String())
		return testErrors
	}
	// test failure processing
	s.Logger.Log("test step failed", s.String())
	if s.Assert == nil {
		return testErrors
	}
	for _, collector := range s.Assert.Collectors {
		s.Logger.Logf("collecting log output for %s", collector.String())
		if collector.Command() == nil {
			s.Logger.Log("skipping invalid assertion collector")
			continue
		}
		_, err := testutils.RunCommand(context.TODO(), namespace, *collector.Command(), s.Dir, s.Logger, s.Logger, s.Logger, s.Timeout, s.Kubeconfig)
		if err != nil {
			s.Logger.Log("post assert collector failure: %s", err)
		}
	}
	s.Logger.Flush()
	return testErrors
}

// String implements the string interface, returning the name of the test step.
func (s *Step) String() string {
	return fmt.Sprintf("%d-%s", s.Index, s.Name)
}

// LoadYAML loads the resources from a YAML file for a test step:
// * If the YAML file is called "assert", then it contains objects to
//   add to the test step's list of assertions.
// * If the YAML file is called "errors", then it contains objects that,
//   if seen, mark a test immediately failed.
// * All other YAML files are considered resources to create.
func (s *Step) LoadYAML(file string, templatingContext testutils.TemplatingContext) error {
	objects, err := testutils.LoadYAMLFromFile(file, templatingContext)
	if err != nil {
		return fmt.Errorf("loading %s: %s", file, err)
	}

	if err = s.populateObjectsByFileName(filepath.Base(file), objects); err != nil {
		return fmt.Errorf("populating step: %v", err)
	}

	asserts := []client.Object{}

	for _, obj := range s.Asserts {
		if obj.GetObjectKind().GroupVersionKind().Kind == "TestAssert" {
			if testAssert, ok := obj.DeepCopyObject().(*harness.TestAssert); ok {
				s.Assert = testAssert
			} else {
				return fmt.Errorf("failed to load TestAssert object from %s: it contains an object of type %T", file, obj)
			}
		} else {
			asserts = append(asserts, obj)
		}
	}

	applies := []client.Object{}

	for _, obj := range s.Apply {
		if obj.GetObjectKind().GroupVersionKind().Kind == "TestStep" {
			if testStep, ok := obj.(*harness.TestStep); ok {
				if s.Step != nil {
					return fmt.Errorf("more than 1 TestStep not allowed in step %q", s.Name)
				}
				s.Step = testStep
			} else {
				return fmt.Errorf("failed to load TestStep object from %s: it contains an object of type %T", file, obj)
			}
			s.Step.Index = s.Index
			if s.Step.Name != "" {
				s.Name = s.Step.Name
			}
			if s.Step.Kubeconfig != "" {
				exKubeconfig := env.Expand(s.Step.Kubeconfig)
				s.Kubeconfig = cleanPath(exKubeconfig, s.Dir)
			}
		} else {
			applies = append(applies, obj)
		}
	}

	// process provided steps configured TestStep kind
	if s.Step != nil {
		// process configured step applies
		for _, applyPath := range s.Step.Apply {
			exApply := env.Expand(applyPath)
			apply, err := ObjectsFromPath(exApply, s.Dir, templatingContext)
			if err != nil {
				return fmt.Errorf("step %q apply path %s: %w", s.Name, exApply, err)
			}
			applies = append(applies, apply...)
		}
		// process configured step asserts
		for _, assertPath := range s.Step.Assert {
			exAssert := env.Expand(assertPath)
			assert, err := ObjectsFromPath(exAssert, s.Dir, templatingContext)
			if err != nil {
				return fmt.Errorf("step %q assert path %s: %w", s.Name, exAssert, err)
			}
			asserts = append(asserts, assert...)
		}
		// process configured errors
		for _, errorPath := range s.Step.Error {
			exError := env.Expand(errorPath)
			errObjs, err := ObjectsFromPath(exError, s.Dir, templatingContext)
			if err != nil {
				return fmt.Errorf("step %q error path %s: %w", s.Name, exError, err)
			}
			s.Errors = append(s.Errors, errObjs...)
		}
	}

	s.Apply = applies
	s.Asserts = asserts
	return nil
}

// populateObjectsByFileName populates s.Asserts, s.Errors, and/or s.Apply for files containing
// "assert", "errors", or no special string, respectively.
func (s *Step) populateObjectsByFileName(fileName string, objects []client.Object) error {
	matches := fileNameRegex.FindStringSubmatch(fileName)
	if len(matches) < 2 {
		return fmt.Errorf("%s does not match file name regexp: %s", fileName, fileNameRegex.String())
	}

	switch fname := strings.ToLower(matches[1]); fname {
	case "assert":
		s.Asserts = append(s.Asserts, objects...)
	case "errors":
		s.Errors = append(s.Errors, objects...)
	default:
		if s.Name == "" {
			if len(matches) > 2 {
				// The second matching group will already have a hyphen prefix.
				s.Name = matches[1] + matches[2]
			} else {
				s.Name = matches[1]
			}
		}
		s.Apply = append(s.Apply, objects...)
	}

	return nil
}

// ObjectsFromPath returns an array of runtime.Objects for files / urls provided
func ObjectsFromPath(path, dir string, templatingContext testutils.TemplatingContext) ([]client.Object, error) {
	if http.IsURL(path) {
		apply, err := http.ToObjects(path)
		if err != nil {
			return nil, err
		}
		return apply, nil
	}

	// it's a directory or file
	cPath := cleanPath(path, dir)
	paths, err := kfile.FromPath(cPath, "*.yaml")
	if err != nil {
		return nil, fmt.Errorf("failed to find YAML files in %s: %w", cPath, err)
	}
	apply, err := kfile.ToObjects(paths, templatingContext)
	if err != nil {
		return nil, err
	}
	return apply, nil
}

// cleanPath returns either the abs path or the joined path
func cleanPath(path, dir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

func hasTimeoutErr(err []error) bool {
	for i := range err {
		if errors.Is(err[i], context.DeadlineExceeded) {
			return true
		}
	}
	return false
}
