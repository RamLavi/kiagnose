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

package checkup

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"

	"github.com/kiagnose/kiagnose/kiagnose/configmap"
	"github.com/kiagnose/kiagnose/kiagnose/internal/checkup/job"
	"github.com/kiagnose/kiagnose/kiagnose/internal/config"
	"github.com/kiagnose/kiagnose/kiagnose/internal/rbac"
	"github.com/kiagnose/kiagnose/kiagnose/internal/results"
)

type Checkup struct {
	client          kubernetes.Interface
	teardownTimeout time.Duration
	resultConfigMap *corev1.ConfigMap
	roles           []*rbacv1.Role
	roleBindings    []*rbacv1.RoleBinding
	jobTimeout      time.Duration
	job             *batchv1.Job
}

const (
	UIDEnvVarName                       = "CHECKUP_UID"
	ResultsConfigMapNameEnvVarName      = "RESULT_CONFIGMAP_NAME"
	ResultsConfigMapNameEnvVarNamespace = "RESULT_CONFIGMAP_NAMESPACE"
)

func New(c kubernetes.Interface, targetNsName, name string, checkupConfig *config.Config) *Checkup {
	resultsConfigMapName := NameResultsConfigMap(name)
	resultsConfigMapWriterRoleName := NameResultsConfigMapWriterRole(name)
	jobName := NameJob(name)
	checkupRoles := []*rbacv1.Role{NewConfigMapWriterRole(targetNsName, resultsConfigMapWriterRoleName, resultsConfigMapName)}

	serviceAccountSubject := NewServiceAccountSubject(targetNsName, checkupConfig.ServiceAccountName)
	var checkupRoleBindings []*rbacv1.RoleBinding
	for _, role := range checkupRoles {
		checkupRoleBindings = append(checkupRoleBindings, NewRoleBinding(targetNsName, role.Name, serviceAccountSubject))
	}

	checkupEnvVars := []corev1.EnvVar{
		{Name: UIDEnvVarName, Value: checkupConfig.UID},
		{Name: ResultsConfigMapNameEnvVarName, Value: resultsConfigMapName},
		{Name: ResultsConfigMapNameEnvVarNamespace, Value: targetNsName},
	}
	checkupEnvVars = append(checkupEnvVars, checkupConfig.EnvVars...)

	const defaultTeardownTimeout = time.Minute * 5
	return &Checkup{
		client:          c,
		teardownTimeout: defaultTeardownTimeout,
		resultConfigMap: NewConfigMap(targetNsName, resultsConfigMapName),
		roles:           checkupRoles,
		roleBindings:    checkupRoleBindings,
		jobTimeout:      checkupConfig.Timeout,
		job: NewCheckupJob(
			targetNsName,
			jobName,
			checkupConfig.ServiceAccountName,
			checkupConfig.Image,
			int64(checkupConfig.Timeout.Seconds()),
			checkupEnvVars,
		),
	}
}

// Setup creates each of the checkup objects inside the cluster.
// In case of failure, an attempt to clean up the objects that already been created is made,
// by deleting the Namespace and eventually all the objects inside it
// https://kubernetes.io/docs/concepts/architecture/garbage-collection/#background-deletion
func (c *Checkup) Setup() error {
	const errPrefix = "setup"
	var err error

	if c.resultConfigMap, err = configmap.Create(c.client, c.resultConfigMap); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}

	if c.roles, err = rbac.CreateRoles(c.client, c.roles); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}

	if c.roleBindings, err = rbac.CreateRoleBindings(c.client, c.roleBindings); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}

	return nil
}

func (c *Checkup) Run() error {
	const errPrefix = "run"
	var err error

	if c.job, err = job.Create(c.client, c.job); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}

	var updatedJob *batchv1.Job
	if updatedJob, err = job.WaitForJobToFinish(c.client, c.job, c.jobTimeout); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}
	c.job = updatedJob

	return nil
}

func (c *Checkup) Results() (results.Results, error) {
	return results.ReadFromConfigMap(c.client, c.resultConfigMap.Namespace, c.resultConfigMap.Name)
}

func (c *Checkup) Logs() error {
	if c.job == nil {
		return fmt.Errorf("job is nil")
	}

	var err error
	var logs string
	if logs, err = job.GetLogs(c.client, c.job); err != nil {
		return err
	}
	log.Printf("checkup job %q Logs:\n%s\n", c.job.Name, logs)
	return nil
}

func (c *Checkup) SetTeardownTimeout(duration time.Duration) {
	c.teardownTimeout = duration
}

func (c *Checkup) Teardown() error {
	var errs []error

	if c.job != nil {
		if err := job.DeleteAndWait(c.client, c.job, c.teardownTimeout); err != nil {
			errs = append(errs, err)
		}
	}

	if err := rbac.DeleteRoleBindings(c.client, c.roleBindings); err != nil {
		errs = append(errs, err)
	}

	if err := rbac.DeleteRoles(c.client, c.roles); err != nil {
		errs = append(errs, err)
	}

	if err := configmap.Delete(c.client, c.resultConfigMap.Namespace, c.resultConfigMap.Name); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		const errPrefix = "teardown"
		return fmt.Errorf("%s: %v", errPrefix, concentrateErrors(errs))
	}

	return nil
}

func NewConfigMap(namespaceName, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespaceName},
	}
}

func NewConfigMapWriterRole(namespaceName, name, configMapName string) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{Kind: "Role", APIVersion: rbacv1.GroupName},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespaceName},
		Rules:      []rbacv1.PolicyRule{newConfigMapWriterPolicyRule(configMapName)},
	}
}

func newConfigMapWriterPolicyRule(cmName string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{
		Verbs:         []string{"get", "update", "patch"},
		APIGroups:     []string{""},
		Resources:     []string{"configmaps"},
		ResourceNames: []string{cmName},
	}
}

func NewRoleBinding(namespaceName, roleName string, subject rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{Kind: "RoleBinding", APIVersion: rbacv1.GroupName},
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespaceName},
		Subjects:   []rbacv1.Subject{subject},
		RoleRef:    rbacv1.RoleRef{Kind: "Role", APIGroup: rbacv1.GroupName, Name: roleName},
	}
}

func NewServiceAccountSubject(serviceAccountNamespace, serviceAccountName string) rbacv1.Subject {
	return rbacv1.Subject{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      serviceAccountName,
		Namespace: serviceAccountNamespace,
	}
}

func NewCheckupJob(namespaceName, name, serviceAccountName, image string, activeDeadlineSeconds int64, envs []corev1.EnvVar) *batchv1.Job {
	const containerName = "checkup"

	checkupContainer := corev1.Container{Name: containerName, Image: image, Env: envs}
	var defaultTerminationGracePeriodSeconds int64 = 5
	checkupPodSpec := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: corev1.PodSpec{
			ServiceAccountName:            serviceAccountName,
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &defaultTerminationGracePeriodSeconds,
			Containers:                    []corev1.Container{checkupContainer},
		},
	}
	var backoffLimit int32 = 0
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespaceName},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &activeDeadlineSeconds,
			Template:              checkupPodSpec,
		},
	}
}

func NameResultsConfigMap(checkupName string) string {
	return checkupName + "-results"
}

func NameResultsConfigMapWriterRole(checkupName string) string {
	return checkupName + "-results-cm-writer"
}

func NameJob(checkupName string) string {
	return checkupName + "-checkup"
}

func concentrateErrors(errs []error) error {
	sb := strings.Builder{}
	for _, err := range errs {
		sb.WriteString(err.Error())
		sb.WriteString("\n")
	}

	return errors.New(sb.String())
}
