package argocd

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceProviderConditionReady is the condition type used when reporting status
const (
	ServiceProviderConditionReady = "Ready"
	StatusPhaseFailed             = "Failed"
)

// API defines the interface that the object must implement to set status.
type API interface {
	GetConditions() *[]metav1.Condition
	GetGeneration() int64
	SetObservedGeneration(generation int64)
	SetPhase(phase string)
}

func StatusFailed(obj API, reason, message string) {
	meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
		Type:               ServiceProviderConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             reason,
		Message:            message,
	})
	obj.SetObservedGeneration(obj.GetGeneration())
	obj.SetPhase(StatusPhaseFailed)
}
