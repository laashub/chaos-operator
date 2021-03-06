// Copyright 2018 The Operator-SDK Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	goctx "context"
	"fmt"
	"os"
	"sync"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	k8sInternal "github.com/operator-framework/operator-sdk/internal/util/k8sutil"

	extscheme "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	cached "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/kubernetes"
	cgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	dynclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// Global framework struct
	Global *Framework
	// mutex for AddToFrameworkScheme
	mutex = sync.Mutex{}
	// whether to run tests in a single namespace
	singleNamespace *bool
	// decoder used by createFromYaml
	dynamicDecoder runtime.Decoder
	// restMapper for the dynamic client
	restMapper *restmapper.DeferredDiscoveryRESTMapper
)

type Framework struct {
	Client            *frameworkClient
	KubeConfig        *rest.Config
	KubeClient        kubernetes.Interface
	Scheme            *runtime.Scheme
	NamespacedManPath *string
	Namespace         string
	LocalOperator     bool
}

func setup(kubeconfigPath, namespacedManPath *string, localOperator bool) error {
	namespace := ""
	if *singleNamespace {
		namespace = os.Getenv(TestNamespaceEnv)
	}
	var err error
	var kubeconfig *rest.Config
	var kcNamespace string
	kubeconfig, kcNamespace, err = k8sInternal.GetKubeconfigAndNamespace(*kubeconfigPath)
	if *singleNamespace && namespace == "" {
		namespace = kcNamespace
	}
	if err != nil {
		return fmt.Errorf("failed to build the kubeconfig: %v", err)
	}
	kubeclient, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build the kubeclient: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := cgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to add cgo scheme to runtime scheme: (%v)", err)
	}
	if err := extscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to add api extensions scheme to runtime scheme: (%v)", err)
	}
	cachedDiscoveryClient := cached.NewMemCacheClient(kubeclient.Discovery())
	restMapper = restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)
	restMapper.Reset()
	dynClient, err := dynclient.New(kubeconfig, dynclient.Options{Scheme: scheme, Mapper: restMapper})
	if err != nil {
		return fmt.Errorf("failed to build the dynamic client: %v", err)
	}
	dynamicDecoder = serializer.NewCodecFactory(scheme).UniversalDeserializer()
	Global = &Framework{
		Client:            &frameworkClient{Client: dynClient},
		KubeConfig:        kubeconfig,
		KubeClient:        kubeclient,
		Scheme:            scheme,
		NamespacedManPath: namespacedManPath,
		Namespace:         namespace,
		LocalOperator:     localOperator,
	}
	return nil
}

type addToSchemeFunc func(*runtime.Scheme) error

// AddToFrameworkScheme allows users to add the scheme for their custom resources
// to the framework's scheme for use with the dynamic client. The user provides
// the addToScheme function (located in the register.go file of their operator
// project) and the List struct for their custom resource. For example, for a
// memcached operator, the list stuct may look like:
// &MemcachedList{}
// The List object is needed because the CRD has not always been fully registered
// by the time this function is called. If the CRD takes more than 5 seconds to
// become ready, this function throws an error
func AddToFrameworkScheme(addToScheme addToSchemeFunc, obj runtime.Object) error {
	mutex.Lock()
	defer mutex.Unlock()
	err := addToScheme(Global.Scheme)
	if err != nil {
		return err
	}
	restMapper.Reset()
	dynClient, err := dynclient.New(Global.KubeConfig, dynclient.Options{Scheme: Global.Scheme, Mapper: restMapper})
	if err != nil {
		return fmt.Errorf("failed to initialize new dynamic client: (%v)", err)
	}
	err = wait.PollImmediate(time.Second, time.Second*10, func() (done bool, err error) {
		if *singleNamespace {
			err = dynClient.List(goctx.TODO(), obj, dynclient.InNamespace(Global.Namespace))
		} else {
			err = dynClient.List(goctx.TODO(), obj, dynclient.InNamespace("default"))
		}
		if err != nil {
			restMapper.Reset()
			return false, nil
		}
		Global.Client = &frameworkClient{Client: dynClient}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("failed to build the dynamic client: %v", err)
	}
	dynamicDecoder = serializer.NewCodecFactory(Global.Scheme).UniversalDeserializer()
	return nil
}
