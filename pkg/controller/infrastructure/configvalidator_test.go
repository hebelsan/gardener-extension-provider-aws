// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infrastructure_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws/awserr"
	apisaws "github.com/gardener/gardener-extension-provider-aws/pkg/apis/aws"
	"github.com/gardener/gardener-extension-provider-aws/pkg/aws"
	mockawsclient "github.com/gardener/gardener-extension-provider-aws/pkg/aws/client/mock"
	. "github.com/gardener/gardener-extension-provider-aws/pkg/controller/infrastructure"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener/extensions/pkg/controller/infrastructure"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	mockclient "github.com/gardener/gardener/pkg/mock/controller-runtime/client"
	. "github.com/gardener/gardener/pkg/utils/test/matchers"
	"github.com/go-logr/logr"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
)

const (
	name      = "infrastructure"
	namespace = "shoot--foobar--aws"
	region    = "eu-west-1"
	vpcID     = "vpc-123456"

	accessKeyID     = "accessKeyID"
	secretAccessKey = "secretAccessKey"
)

var _ = Describe("ConfigValidator", func() {
	var (
		ctrl             *gomock.Controller
		c                *mockclient.MockClient
		awsClientFactory *mockawsclient.MockFactory
		awsClient        *mockawsclient.MockInterface
		ctx              context.Context
		logger           logr.Logger
		cv               infrastructure.ConfigValidator
		infra            *extensionsv1alpha1.Infrastructure
		secret           *corev1.Secret
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())

		c = mockclient.NewMockClient(ctrl)
		awsClientFactory = mockawsclient.NewMockFactory(ctrl)
		awsClient = mockawsclient.NewMockInterface(ctrl)

		ctx = context.TODO()
		logger = log.Log.WithName("test")

		cv = NewConfigValidator(awsClientFactory, logger)
		err := cv.(inject.Client).InjectClient(c)
		Expect(err).NotTo(HaveOccurred())

		infra = &extensionsv1alpha1.Infrastructure{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: extensionsv1alpha1.InfrastructureSpec{
				DefaultSpec: extensionsv1alpha1.DefaultSpec{
					Type: aws.Type,
					ProviderConfig: &runtime.RawExtension{
						Raw: encode(&apisaws.InfrastructureConfig{
							Networks: apisaws.Networks{
								VPC: apisaws.VPC{
									ID: pointer.String(vpcID),
								},
							},
						}),
					},
				},
				Region: region,
				SecretRef: corev1.SecretReference{
					Name:      name,
					Namespace: namespace,
				},
			},
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				aws.AccessKeyID:     []byte(accessKeyID),
				aws.SecretAccessKey: []byte(secretAccessKey),
			},
		}
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	Describe("#Validate", func() {
		BeforeEach(func() {
			c.EXPECT().Get(ctx, kutil.Key(namespace, name), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, obj *corev1.Secret) error {
					*obj = *secret
					return nil
				},
			)
			awsClientFactory.EXPECT().NewClient(accessKeyID, secretAccessKey, region).Return(awsClient, nil)
		})

		It("should forbid VPC that doesn't exist", func() {
			awsClient.EXPECT().GetVPCAttribute(ctx, vpcID, "enableDnsSupport").Return(false, awserr.New("InvalidVpcID.NotFound", "", nil))

			errorList := cv.Validate(ctx, infra)
			Expect(errorList).To(ConsistOfFields(Fields{
				"Type":  Equal(field.ErrorTypeNotFound),
				"Field": Equal("networks.vpc.id"),
			}))
		})

		It("should forbid VPC that exists but has wrong attribute values or no attached internet gateway", func() {
			awsClient.EXPECT().GetVPCAttribute(ctx, vpcID, "enableDnsSupport").Return(false, nil)
			awsClient.EXPECT().GetVPCAttribute(ctx, vpcID, "enableDnsHostnames").Return(false, nil)
			awsClient.EXPECT().GetVPCInternetGateway(ctx, vpcID).Return("", nil)

			errorList := cv.Validate(ctx, infra)
			Expect(errorList).To(ConsistOfFields(Fields{
				"Type":   Equal(field.ErrorTypeInvalid),
				"Field":  Equal("networks.vpc.id"),
				"Detail": Equal("VPC attribute enableDnsSupport must be set to true"),
			}, Fields{
				"Type":   Equal(field.ErrorTypeInvalid),
				"Field":  Equal("networks.vpc.id"),
				"Detail": Equal("VPC attribute enableDnsHostnames must be set to true"),
			}, Fields{
				"Type":   Equal(field.ErrorTypeInvalid),
				"Field":  Equal("networks.vpc.id"),
				"Detail": Equal("no attached internet gateway found"),
			}))
		})

		It("should allow VPC that exists and has correct attribute values and an attached internet gateway", func() {
			awsClient.EXPECT().GetVPCAttribute(ctx, vpcID, "enableDnsSupport").Return(true, nil)
			awsClient.EXPECT().GetVPCAttribute(ctx, vpcID, "enableDnsHostnames").Return(true, nil)
			awsClient.EXPECT().GetVPCInternetGateway(ctx, vpcID).Return(vpcID, nil)

			errorList := cv.Validate(ctx, infra)
			Expect(errorList).To(BeEmpty())
		})

		It("should fail with InternalError if getting VPC attributes failed", func() {
			awsClient.EXPECT().GetVPCAttribute(ctx, vpcID, "enableDnsSupport").Return(false, errors.New("test"))

			errorList := cv.Validate(ctx, infra)
			Expect(errorList).To(ConsistOfFields(Fields{
				"Type":   Equal(field.ErrorTypeInternal),
				"Field":  Equal("networks.vpc.id"),
				"Detail": Equal(fmt.Sprintf("could not get VPC attribute enableDnsSupport for VPC %s: test", vpcID)),
			}))
		})

		Describe("validate Elastic IP addresses", func() {
			BeforeEach(func() {
				infra.ClusterName = "cluster-1"
				infra.Spec.ProviderConfig.Raw = encode(&apisaws.InfrastructureConfig{
					Networks: apisaws.Networks{
						VPC: apisaws.VPC{},
						Zones: []apisaws.Zone{
							{
								ElasticIPAllocationID: pointer.String("eipalloc-0e2669d4b46150ee4"),
							},
							{
								ElasticIPAllocationID: pointer.String("eipalloc-0e2669d4b46150ee5"),
							},
							{
								ElasticIPAllocationID: pointer.String("eipalloc-0e2669d4b46150ee6"),
							},
						},
					},
				})
			})

			It("should succeed - no EIPs configured", func() {
				infra.Spec.ProviderConfig.Raw = encode(&apisaws.InfrastructureConfig{
					Networks: apisaws.Networks{
						VPC: apisaws.VPC{},
					},
				})
				errorList := cv.Validate(ctx, infra)
				Expect(errorList).To(BeEmpty())
			})

			It("should succeed - all EIPs exist and are already associated to the Shoot's NAT Gateways", func() {
				mapping := map[string]*string{
					"eipalloc-0e2669d4b46150ee4": pointer.String("eipassoc-0f8ff66536587824b"),
					"eipalloc-0e2669d4b46150ee5": pointer.String("eipassoc-0f8ff66536587824c"),
					"eipalloc-0e2669d4b46150ee6": pointer.String("eipassoc-0f8ff66536587824d"),
				}
				awsClient.EXPECT().GetElasticIPsAssociationIDForAllocationIDs(ctx, gomock.Any()).Return(mapping, nil)
				awsClient.EXPECT().GetNATGatewayAddressAllocations(ctx, infra.ClusterName).Return(sets.NewString("eipalloc-0e2669d4b46150ee4", "eipalloc-0e2669d4b46150ee5", "eipalloc-0e2669d4b46150ee6"), nil)

				errorList := cv.Validate(ctx, infra)
				Expect(errorList).To(BeEmpty())
			})

			It("should succeed - all EIPs exist, but are not associated to any resource yet", func() {
				mapping := map[string]*string{
					"eipalloc-0e2669d4b46150ee4": nil,
					"eipalloc-0e2669d4b46150ee5": nil,
					"eipalloc-0e2669d4b46150ee6": nil,
				}
				awsClient.EXPECT().GetElasticIPsAssociationIDForAllocationIDs(ctx, gomock.Any()).Return(mapping, nil)

				errorList := cv.Validate(ctx, infra)
				Expect(errorList).To(BeEmpty())
			})

			It("should fail - the Elastic IP Address for the given allocation ID does not exist", func() {
				empty := make(map[string]*string, 0)
				awsClient.EXPECT().GetElasticIPsAssociationIDForAllocationIDs(ctx, []string{"eipalloc-0e2669d4b46150ee4", "eipalloc-0e2669d4b46150ee5", "eipalloc-0e2669d4b46150ee6"}).Return(empty, nil)

				errorList := cv.Validate(ctx, infra)
				Expect(errorList).To(ConsistOfFields(Fields{
					"Type":     Equal(field.ErrorTypeInvalid),
					"Field":    Equal("networks.zones[].elasticIPAllocationID"),
					"BadValue": Equal("eipalloc-0e2669d4b46150ee4"),
					"Detail":   ContainSubstring("cannot be used as it does not exist"),
				}, Fields{
					"Type":     Equal(field.ErrorTypeInvalid),
					"Field":    Equal("networks.zones[].elasticIPAllocationID"),
					"BadValue": Equal("eipalloc-0e2669d4b46150ee5"),
					"Detail":   ContainSubstring("cannot be used as it does not exist"),
				}, Fields{
					"Type":     Equal(field.ErrorTypeInvalid),
					"Field":    Equal("networks.zones[].elasticIPAllocationID"),
					"BadValue": Equal("eipalloc-0e2669d4b46150ee6"),
					"Detail":   ContainSubstring("cannot be used as it does not exist"),
				},
				))
			})

			It("should fail - some of the Elastic IP Addresses exist, some do not", func() {
				mapping := map[string]*string{
					"eipalloc-0e2669d4b46150ee4": pointer.String("eipassoc-0f8ff66536587824b"),
					"eipalloc-0e2669d4b46150ee5": pointer.String("eipassoc-0f8ff66536587824c"),
				}
				awsClient.EXPECT().GetElasticIPsAssociationIDForAllocationIDs(ctx, gomock.Any()).Return(mapping, nil)
				awsClient.EXPECT().GetNATGatewayAddressAllocations(ctx, infra.ClusterName).Return(sets.NewString("eipalloc-0e2669d4b46150ee4", "eipalloc-0e2669d4b46150ee5"), nil)

				errorList := cv.Validate(ctx, infra)
				Expect(errorList).To(ConsistOfFields(Fields{
					"Type":     Equal(field.ErrorTypeInvalid),
					"Field":    Equal("networks.zones[].elasticIPAllocationID"),
					"BadValue": Equal("eipalloc-0e2669d4b46150ee6"),
					"Detail":   ContainSubstring("cannot be used as it does not exist"),
				}))
			})

			It("should fail - Elastic IP Addresses exist are already associated with another resource", func() {
				mapping := map[string]*string{
					"eipalloc-0e2669d4b46150ee4": pointer.String("eipassoc-0f8ff66536587824b"),
					"eipalloc-0e2669d4b46150ee5": pointer.String("eipassoc-0f8ff66536587824c"),
					"eipalloc-0e2669d4b46150ee6": pointer.String("eipassoc-0f8ff66536587824d"),
				}
				awsClient.EXPECT().GetElasticIPsAssociationIDForAllocationIDs(ctx, gomock.Any()).Return(mapping, nil)
				awsClient.EXPECT().GetNATGatewayAddressAllocations(ctx, infra.ClusterName).Return(sets.NewString("eipalloc-0e2669d4b46150ee4", "eipalloc-0e2669d4b46150ee5"), nil)

				errorList := cv.Validate(ctx, infra)
				Expect(errorList).To(ConsistOfFields(Fields{
					"Type":     Equal(field.ErrorTypeInvalid),
					"Field":    Equal("networks.zones[].elasticIPAllocationID"),
					"BadValue": Equal("eipalloc-0e2669d4b46150ee6"),
					"Detail":   ContainSubstring("cannot be attached to the clusters NAT Gateway(s) as it is already associated"),
				}))
			})
		})
	})
})

func encode(obj runtime.Object) []byte {
	data, _ := json.Marshal(obj)
	return data
}
