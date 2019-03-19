/*
Copyright 2018 The Kubernetes Authors.

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

package keystone

import (
	"errors"
	"fmt"
	"io/ioutil"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v2"
	core_v1 "k8s.io/api/core/v1"
	rbac_v1 "k8s.io/api/rbac/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
)

// By now only project syncing is supported
// TODO(mfedosin): Implement syncing of role assignments, system role assignments, and user groups
const (
	Projects        = "projects"
	RoleAssignments = "role_assignments"
)

var allowedDataTypesToSync = []string{Projects, RoleAssignments}

// syncConfig contains configuration data for synchronization between Keystone and Kubernetes
type syncConfig struct {
	// List containing possible data types to sync. Now only "projects" are supported.
	DataTypesToSync []string `yaml:"data_types_to_sync"`

	// Format of automatically created namespace name. Can contain wildcards %i and %n,
	// corresponding to project id and project name respectively.
	NamespaceFormat string `yaml:"namespace_format"`

	// List of project ids to exclude from syncing.
	ProjectBlackList []string `yaml:"projects_black_list"`
}

func (sc *syncConfig) validate() error {
	// Namespace name must contain keystone project id
	if !strings.Contains(sc.NamespaceFormat, "%i") {
		return fmt.Errorf("format string should comprise a %%i substring (keystone project id)")
	}

	// By convention, the names should be up to maximum length of 63 characters and consist of
	// lower and upper case alphanumeric characters, -, _ and .
	ts := strings.Replace(sc.NamespaceFormat, "%i", "aa", -1)
	ts = strings.Replace(ts, "%n", "aa", -1)
	ts = strings.Replace(ts, "%d", "aa", -1)

	re := regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9_.-]*[a-zA-Z0-9]$")
	if !re.MatchString(ts) {
		return fmt.Errorf("namespace name must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character")
	}

	// Check that only allowed data types are enabled for synchronization
	for _, dt := range sc.DataTypesToSync {
		var flag bool
		for _, a := range allowedDataTypesToSync {
			if a == dt {
				flag = true
				break
			}
		}
		if !flag {
			return fmt.Errorf(
				"Unsupported data type to sync: %v. Available values: %v",
				dt,
				strings.Join(allowedDataTypesToSync, ","),
			)
		}
	}

	return nil
}

// formatNamespaceName generates a namespace name, based on format string
func (sc *syncConfig) formatNamespaceName(id string, name string, domain string) string {
	res := strings.Replace(sc.NamespaceFormat, "%i", id, -1)
	res = strings.Replace(res, "%n", name, -1)
	res = strings.Replace(res, "%d", domain, -1)

	if len(res) > 63 {
		klog.Warningf("Generated namespace name '%v' exceeds the maximum possible length of 63 characters. Just Keystone project id '%v' will be used as the namespace name.", res, id)
		return id
	}

	return res
}

// newSyncConfig defines the default values for syncConfig
func newSyncConfig() syncConfig {
	return syncConfig{
		// by default namespace name is a string containing just keystone project id
		NamespaceFormat: "%i",
		// by default all possible data types are enabled
		DataTypesToSync: allowedDataTypesToSync,
	}
}

// newSyncConfigFromFile loads a sync config from a file
func newSyncConfigFromFile(path string) (*syncConfig, error) {
	sc := newSyncConfig()

	yamlFile, err := ioutil.ReadFile(path)
	if err != nil {
		klog.Errorf("yamlFile get err   #%v ", err)
		return nil, err
	}
	err = yaml.Unmarshal(yamlFile, &sc)
	if err != nil {
		klog.Errorf("Unmarshal: %v", err)
		return nil, err
	}

	return &sc, nil
}

// Syncer synchronizes auth data between Keystone and Kubernetes
type Syncer struct {
	k8sClient  *kubernetes.Clientset
	syncConfig *syncConfig
	mu         sync.Mutex
}

func (s *Syncer) syncData(u *userInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range s.syncConfig.ProjectBlackList {
		if u.Extra["alpha.kubernetes.io/identity/project/id"][0] == p {
			klog.Infof("Project %v is in black list. Skipping.", p)
			return nil
		}
	}

	if s.k8sClient == nil {
		return errors.New("cannot sync data because k8s client is not initialized")
	}

	namespaceName := s.syncConfig.formatNamespaceName(
		u.Extra["alpha.kubernetes.io/identity/project/id"][0],
		u.Extra["alpha.kubernetes.io/identity/project/name"][0],
		u.Extra["alpha.kubernetes.io/identity/user/domain/id"][0],
	)

	// sync project data first
	for _, dataType := range s.syncConfig.DataTypesToSync {
		if dataType == Projects {
			err := s.syncProjectData(u, namespaceName)
			if err != nil {
				return err
			}
		}
	}

	for _, dataType := range s.syncConfig.DataTypesToSync {
		if dataType == RoleAssignments {
			err := s.syncRoleAssignmentsData(u, namespaceName)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Syncer) syncProjectData(u *userInfo, namespaceName string) error {
	_, err := s.k8sClient.CoreV1().Namespaces().Get(namespaceName, metav1.GetOptions{})

	if k8s_errors.IsNotFound(err) {
		// The required namespace is not found. Create it then.
		namespace := &core_v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespaceName,
			},
		}
		namespace, err = s.k8sClient.CoreV1().Namespaces().Create(namespace)
		if err != nil {
			klog.Warningf("Cannot create a namespace for the user: %v", err)
			return errors.New("Internal server error")
		}
	} else if err != nil {
		// Some other error.
		klog.Warningf("Cannot get a response from the server: %v", err)
		return errors.New("Internal server error")
	}

	return nil
}

func (s *Syncer) syncRoleAssignmentsData(u *userInfo, namespaceName string) error {
	// TODO(mfedosin): add a field separator to filter out unnecessary roles bindings at an early stage
	roleBindings, err := s.k8sClient.RbacV1().RoleBindings(namespaceName).List(metav1.ListOptions{})
	if err != nil {
		klog.Warningf("Cannot get a list of role bindings from the server: %v", err)
		return errors.New("Internal server error")
	}

	// delete role bindings removed from Keystone
	for _, roleBinding := range roleBindings.Items {
		// parts[0] is a user id, parts[1] is a role name
		parts := strings.SplitN(roleBinding.Name, "_", 2)
		if len(parts) == 1 || parts[0] != u.UID {
			// role binding is either created by an admin or belongs to a different user
			continue
		}

		var keepRoleBinding bool
		for _, roleName := range u.Extra["alpha.kubernetes.io/identity/roles"] {
			roleBindingName := u.UID + "_" + roleName
			if roleBinding.Name == roleBindingName {
				keepRoleBinding = true
			}
		}
		if !keepRoleBinding {
			err = s.k8sClient.RbacV1().RoleBindings(namespaceName).Delete(roleBinding.Name, &metav1.DeleteOptions{})
			if err != nil {
				klog.Warningf("Cannot delete a role binding from the server: %v", err)
				return errors.New("Internal server error")
			}
		}

	}

	// create new role bindings
	for _, roleName := range u.Extra["alpha.kubernetes.io/identity/roles"] {
		roleBindingName := u.UID + "_" + roleName

		// check that role binding doesn't exist
		var roleBindingExists bool
		for _, roleBinding := range roleBindings.Items {
			if roleBindingName == roleBinding.Name {
				roleBindingExists = true
				break
			}
		}
		if roleBindingExists {
			continue
		}

		roleBinding := &rbac_v1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: roleBindingName,
			},
			Subjects: []rbac_v1.Subject{
				{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "User",
					Name:     u.Username,
				},
			},
			RoleRef: rbac_v1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     roleName,
			},
		}
		roleBinding, err = s.k8sClient.RbacV1().RoleBindings(namespaceName).Create(roleBinding)
		if err != nil {
			klog.Warningf("Cannot create a role binding for the user: %v", err)
			return errors.New("Internal server error")
		}
	}

	return nil
}
