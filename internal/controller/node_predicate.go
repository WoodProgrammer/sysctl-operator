/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// nodeLabelChanged filters Node events down to the ones that can change which
// nodes a profile selects: a node being created, or an existing node's labels
// changing. The frequent status/lease updates a kubelet writes (heartbeats)
// don't touch labels, so they are dropped and never churn the reconcile queue.
// Node deletions are ignored too: a removed node simply drops out of the
// selected set on the next reconcile, so there is nothing to re-apply.
func nodeLabelChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return false
			}
			return !reflect.DeepEqual(oldNode.Labels, newNode.Labels)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
