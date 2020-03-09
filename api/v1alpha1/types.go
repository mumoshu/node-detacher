/*
Copyright 2020 The node-detacher-controller authors.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AttachmentSpec defines the desired state of Attachment
type AttachmentSpec struct {
	// +kubebuilder:validation:MinLength=3
	NodeName string `json:"nodeName"`

	// +optional
	AwsTargets []AwsTarget `json:"awsTargets,omitempty"`

	// +optional
	AwsLoadBalancers []AwsLoadBalancer `json:"awsLoadBalancers,omitempty"`
}

// AwsTarget defines the AWS ELB v2 Target Group Target
type AwsTarget struct {
	ARN string `json:"arn"`

	// +optional
	Port *int64 `json:"port,omitempty"`

	// +optional
	Detached bool `json:"detached,omitempty"`
}

// AwsLoadBalancer defines the AWS ELB v1 CLB that the load-balancing target is attached to
type AwsLoadBalancer struct {
	Name string `json:"name"`

	// +optional
	Detached bool `json:"detached,omitempty"`
}

// AttachmentStatus defines the observed state of Attachment
type AttachmentStatus struct {
	CachedAt   metav1.Time `json:"cachedAt"`
	DetachedAt metav1.Time `json:"detachedAt,omitempty"`
	Phase      string      `json:"phase"`
	Reason     string      `json:"reason"`
	Message    string      `json:"message"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:JSONPath=".spec.nodeName",name=NodeName,type=string
// +kubebuilder:printcolumn:JSONPath=".status.phase",name=Status,type=string

// Attachment is the Schema for the runners API
type Attachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AttachmentSpec   `json:"spec,omitempty"`
	Status AttachmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AttachmentList contains a list of Attachment
type AttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Attachment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Attachment{}, &AttachmentList{})
}
