package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workv1alpha1 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha1"
	"github.com/karmada-io/karmada/pkg/util"
)

// MutatingAdmission mutates API request if necessary.
type MutatingAdmission struct {
	decoder *admission.Decoder
}

// Check if our MutatingAdmission implements necessary interface
var _ admission.Handler = &MutatingAdmission{}
var _ admission.DecoderInjector = &MutatingAdmission{}

// Handle yields a response to an AdmissionRequest.
func (a *MutatingAdmission) Handle(ctx context.Context, req admission.Request) admission.Response {
	work := &workv1alpha1.Work{}

	err := a.decoder.Decode(req, work)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	klog.V(2).Infof("Mutating work(%s) for request: %s", work.Name, req.Operation)

	var manifests []workv1alpha1.Manifest

	for _, manifest := range work.Spec.Workload.Manifests {
		workloadObj := &unstructured.Unstructured{}
		err := json.Unmarshal(manifest.Raw, workloadObj)
		if err != nil {
			klog.Errorf("Failed to unmarshal work(%s) manifest to Unstructured", work.Name)
			return admission.Errored(http.StatusInternalServerError, err)
		}

		err = removeIrrelevantField(workloadObj)
		if err != nil {
			klog.Errorf("Failed to remove irrelevant field for work(%s): %v", work.Name, err)
			return admission.Errored(http.StatusInternalServerError, err)
		}

		workloadJSON, err := workloadObj.MarshalJSON()
		if err != nil {
			klog.Errorf("Failed to marshal workload of work(%s)", work.Name)
			return admission.Errored(http.StatusInternalServerError, err)
		}
		manifests = append(manifests, workv1alpha1.Manifest{RawExtension: runtime.RawExtension{Raw: workloadJSON}})
	}

	work.Spec.Workload.Manifests = manifests
	marshaledBytes, err := json.Marshal(work)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledBytes)
}

// InjectDecoder implements admission.DecoderInjector interface.
// A decoder will be automatically injected.
func (a *MutatingAdmission) InjectDecoder(d *admission.Decoder) error {
	a.decoder = d
	return nil
}

// removeIrrelevantField used to remove fields that generated by kube-apiserver and no need(or can't) propagate to
// member clusters.
func removeIrrelevantField(workload *unstructured.Unstructured) error {
	// populated by the kubernetes.
	unstructured.RemoveNestedField(workload.Object, "metadata", "creationTimestamp")

	// populated by the kubernetes.
	// The kubernetes will set this fields in case of graceful deletion. This field is read-only and can't propagate to
	// member clusters.
	unstructured.RemoveNestedField(workload.Object, "metadata", "deletionTimestamp")

	// populated by the kubernetes.
	// The kubernetes will set this fields in case of graceful deletion. This field is read-only and can't propagate to
	// member clusters.
	unstructured.RemoveNestedField(workload.Object, "metadata", "deletionGracePeriodSeconds")

	// populated by the kubernetes.
	unstructured.RemoveNestedField(workload.Object, "metadata", "generation")

	// This is mostly for internal housekeeping, and users typically shouldn't need to set or understand this field.
	// Remove this field to keep 'Work' clean and tidy.
	unstructured.RemoveNestedField(workload.Object, "metadata", "managedFields")

	// populated by the kubernetes.
	unstructured.RemoveNestedField(workload.Object, "metadata", "resourceVersion")

	// populated by the kubernetes and has been deprecated by kubernetes.
	unstructured.RemoveNestedField(workload.Object, "metadata", "selfLink")

	// populated by the kubernetes.
	unstructured.RemoveNestedField(workload.Object, "metadata", "uid")

	unstructured.RemoveNestedField(workload.Object, "status")

	if workload.GetKind() == util.ServiceKind {
		// In the case spec.clusterIP is set to `None`, means user want a headless service,  then it shouldn't be removed.
		clusterIP, exist, _ := unstructured.NestedString(workload.Object, "spec", "clusterIP")
		if exist && clusterIP != corev1.ClusterIPNone {
			unstructured.RemoveNestedField(workload.Object, "spec", "clusterIP")
		}
		// In the case spec.type is not NodePort, the nodePort in ports will be automatic generated, then remove them
		serviceType, exist, _ := unstructured.NestedString(workload.Object, "spec", "type")
		if exist && serviceType != string(corev1.ServiceTypeNodePort) {
			ports, exist, _ := unstructured.NestedSlice(workload.Object, "spec", "ports")
			if exist && len(ports) > 0 {
				for _, port := range ports {
					unstructured.RemoveNestedField(port.(map[string]interface{}), "nodePort")
				}
			}
		}
	}

	if workload.GetKind() == util.JobKind {
		job := &batchv1.Job{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(workload.UnstructuredContent(), job)
		if err != nil {
			return err
		}

		if job.Spec.ManualSelector == nil || !*job.Spec.ManualSelector {
			return removeGenerateSelectorOfJob(workload)
		}
	}

	if workload.GetKind() == util.ServiceAccountKind {
		secrets, exist, _ := unstructured.NestedSlice(workload.Object, "secrets")
		// If 'secrets' exists in ServiceAccount, remove the automatic generation secrets(e.g. default-token-xxx)
		if exist && len(secrets) > 0 {
			tokenPrefix := fmt.Sprintf("%s-token-", workload.GetName())
			for idx := 0; idx < len(secrets); idx++ {
				if strings.HasPrefix(secrets[idx].(map[string]interface{})["name"].(string), tokenPrefix) {
					secrets = append(secrets[:idx], secrets[idx+1:]...)
				}
			}
			_ = unstructured.SetNestedSlice(workload.Object, secrets, "secrets")
		}
	}

	return nil
}

func removeGenerateSelectorOfJob(workload *unstructured.Unstructured) error {
	matchLabels, exist, err := unstructured.NestedStringMap(workload.Object, "spec", "selector", "matchLabels")
	if err != nil {
		return err
	}
	if exist {
		if util.GetLabelValue(matchLabels, "controller-uid") != "" {
			delete(matchLabels, "controller-uid")
		}
		err = unstructured.SetNestedStringMap(workload.Object, matchLabels, "spec", "selector", "matchLabels")
		if err != nil {
			return err
		}
	}

	templateLabels, exist, err := unstructured.NestedStringMap(workload.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return err
	}
	if exist {
		if util.GetLabelValue(templateLabels, "controller-uid") != "" {
			delete(templateLabels, "controller-uid")
		}

		if util.GetLabelValue(templateLabels, "job-name") != "" {
			delete(templateLabels, "job-name")
		}

		err = unstructured.SetNestedStringMap(workload.Object, templateLabels, "spec", "template", "metadata", "labels")
		if err != nil {
			return err
		}
	}
	return nil
}
