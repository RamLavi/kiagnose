/*
 * This file is part of the kiagnose project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2022 Red Hat, Inc.
 *
 */

package serviceaccount

import (
	"context"
	"log"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

func Create(client corev1client.CoreV1Interface, sa *corev1.ServiceAccount) (*corev1.ServiceAccount, error) {
	createdSa, err := client.ServiceAccounts(sa.Namespace).Create(context.Background(), sa, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	log.Printf("ServiceAccount '%s/%s' successfully created", sa.Namespace, sa.Name)

	return createdSa, nil
}
