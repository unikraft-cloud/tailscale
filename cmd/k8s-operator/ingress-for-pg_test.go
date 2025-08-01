// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"tailscale.com/internal/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	tsoperator "tailscale.com/k8s-operator"
	tsapi "tailscale.com/k8s-operator/apis/v1alpha1"
	"tailscale.com/kube/kubetypes"
	"tailscale.com/tailcfg"
	"tailscale.com/types/ptr"
)

func TestIngressPGReconciler(t *testing.T) {
	ingPGR, fc, ft := setupIngressTest(t)

	ing := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{Kind: "Ingress", APIVersion: "networking.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
			UID:       types.UID("1234-UID"),
			Annotations: map[string]string{
				"tailscale.com/proxy-group": "test-pg",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("tailscale"),
			DefaultBackend: &networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{
					Name: "test",
					Port: networkingv1.ServiceBackendPort{
						Number: 8080,
					},
				},
			},
			TLS: []networkingv1.IngressTLS{
				{Hosts: []string{"my-svc"}},
			},
		},
	}
	mustCreate(t, fc, ing)

	// Verify initial reconciliation
	expectReconciled(t, ingPGR, "default", "test-ingress")
	populateTLSSecret(context.Background(), fc, "test-pg", "my-svc.ts.net")
	expectReconciled(t, ingPGR, "default", "test-ingress")
	verifyServeConfig(t, fc, "svc:my-svc", false)
	verifyTailscaleService(t, ft, "svc:my-svc", []string{"tcp:443"})
	verifyTailscaledConfig(t, fc, []string{"svc:my-svc"})

	// Verify that Role and RoleBinding have been created for the first Ingress.
	// Do not verify the cert Secret as that was already verified implicitly above.
	expectEqual(t, fc, certSecretRole("test-pg", "operator-ns", "my-svc.ts.net"))
	expectEqual(t, fc, certSecretRoleBinding("test-pg", "operator-ns", "my-svc.ts.net"))

	mustUpdate(t, fc, "default", "test-ingress", func(ing *networkingv1.Ingress) {
		ing.Annotations["tailscale.com/tags"] = "tag:custom,tag:test"
	})
	expectReconciled(t, ingPGR, "default", "test-ingress")

	// Verify Tailscale Service uses custom tags
	tsSvc, err := ft.GetVIPService(context.Background(), "svc:my-svc")
	if err != nil {
		t.Fatalf("getting Tailscale Service: %v", err)
	}
	if tsSvc == nil {
		t.Fatal("Tailscale Service not created")
	}
	wantTags := []string{"tag:custom", "tag:test"} // custom tags only
	gotTags := slices.Clone(tsSvc.Tags)
	slices.Sort(gotTags)
	slices.Sort(wantTags)
	if !slices.Equal(gotTags, wantTags) {
		t.Errorf("incorrect Tailscale Service tags: got %v, want %v", gotTags, wantTags)
	}

	// Create second Ingress
	ing2 := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{Kind: "Ingress", APIVersion: "networking.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-other-ingress",
			Namespace: "default",
			UID:       types.UID("5678-UID"),
			Annotations: map[string]string{
				"tailscale.com/proxy-group": "test-pg",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("tailscale"),
			DefaultBackend: &networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{
					Name: "test",
					Port: networkingv1.ServiceBackendPort{
						Number: 8080,
					},
				},
			},
			TLS: []networkingv1.IngressTLS{
				{Hosts: []string{"my-other-svc.tailnetxyz.ts.net"}},
			},
		},
	}
	mustCreate(t, fc, ing2)

	// Verify second Ingress reconciliation
	expectReconciled(t, ingPGR, "default", "my-other-ingress")
	populateTLSSecret(context.Background(), fc, "test-pg", "my-other-svc.ts.net")
	expectReconciled(t, ingPGR, "default", "my-other-ingress")
	verifyServeConfig(t, fc, "svc:my-other-svc", false)
	verifyTailscaleService(t, ft, "svc:my-other-svc", []string{"tcp:443"})

	// Verify that Role and RoleBinding have been created for the first Ingress.
	// Do not verify the cert Secret as that was already verified implicitly above.
	expectEqual(t, fc, certSecretRole("test-pg", "operator-ns", "my-other-svc.ts.net"))
	expectEqual(t, fc, certSecretRoleBinding("test-pg", "operator-ns", "my-other-svc.ts.net"))

	// Verify first Ingress is still working
	verifyServeConfig(t, fc, "svc:my-svc", false)
	verifyTailscaleService(t, ft, "svc:my-svc", []string{"tcp:443"})

	verifyTailscaledConfig(t, fc, []string{"svc:my-svc", "svc:my-other-svc"})

	// Delete second Ingress
	if err := fc.Delete(context.Background(), ing2); err != nil {
		t.Fatalf("deleting second Ingress: %v", err)
	}
	expectReconciled(t, ingPGR, "default", "my-other-ingress")

	// Verify second Ingress cleanup
	cm := &corev1.ConfigMap{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      "test-pg-ingress-config",
		Namespace: "operator-ns",
	}, cm); err != nil {
		t.Fatalf("getting ConfigMap: %v", err)
	}

	cfg := &ipn.ServeConfig{}
	if err := json.Unmarshal(cm.BinaryData[serveConfigKey], cfg); err != nil {
		t.Fatalf("unmarshaling serve config: %v", err)
	}

	// Verify first Ingress is still configured
	if cfg.Services["svc:my-svc"] == nil {
		t.Error("first Ingress service config was incorrectly removed")
	}
	// Verify second Ingress was cleaned up
	if cfg.Services["svc:my-other-svc"] != nil {
		t.Error("second Ingress service config was not cleaned up")
	}

	verifyTailscaledConfig(t, fc, []string{"svc:my-svc"})
	expectMissing[corev1.Secret](t, fc, "operator-ns", "my-other-svc.ts.net")
	expectMissing[rbacv1.Role](t, fc, "operator-ns", "my-other-svc.ts.net")
	expectMissing[rbacv1.RoleBinding](t, fc, "operator-ns", "my-other-svc.ts.net")

	// Delete the first Ingress and verify cleanup
	if err := fc.Delete(context.Background(), ing); err != nil {
		t.Fatalf("deleting Ingress: %v", err)
	}

	expectReconciled(t, ingPGR, "default", "test-ingress")

	// Verify the ConfigMap was cleaned up
	cm = &corev1.ConfigMap{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      "test-pg-ingress-config",
		Namespace: "operator-ns",
	}, cm); err != nil {
		t.Fatalf("getting ConfigMap: %v", err)
	}

	cfg = &ipn.ServeConfig{}
	if err := json.Unmarshal(cm.BinaryData[serveConfigKey], cfg); err != nil {
		t.Fatalf("unmarshaling serve config: %v", err)
	}

	if len(cfg.Services) > 0 {
		t.Error("serve config not cleaned up")
	}
	verifyTailscaledConfig(t, fc, nil)

	// Add verification that cert resources were cleaned up
	expectMissing[corev1.Secret](t, fc, "operator-ns", "my-svc.ts.net")
	expectMissing[rbacv1.Role](t, fc, "operator-ns", "my-svc.ts.net")
	expectMissing[rbacv1.RoleBinding](t, fc, "operator-ns", "my-svc.ts.net")
}

func TestIngressPGReconciler_UpdateIngressHostname(t *testing.T) {
	ingPGR, fc, ft := setupIngressTest(t)

	ing := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{Kind: "Ingress", APIVersion: "networking.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
			UID:       types.UID("1234-UID"),
			Annotations: map[string]string{
				"tailscale.com/proxy-group": "test-pg",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("tailscale"),
			DefaultBackend: &networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{
					Name: "test",
					Port: networkingv1.ServiceBackendPort{
						Number: 8080,
					},
				},
			},
			TLS: []networkingv1.IngressTLS{
				{Hosts: []string{"my-svc"}},
			},
		},
	}
	mustCreate(t, fc, ing)

	// Verify initial reconciliation
	expectReconciled(t, ingPGR, "default", "test-ingress")
	populateTLSSecret(context.Background(), fc, "test-pg", "my-svc.ts.net")
	expectReconciled(t, ingPGR, "default", "test-ingress")
	verifyServeConfig(t, fc, "svc:my-svc", false)
	verifyTailscaleService(t, ft, "svc:my-svc", []string{"tcp:443"})
	verifyTailscaledConfig(t, fc, []string{"svc:my-svc"})

	// Update the Ingress hostname and make sure the original Tailscale Service is deleted.
	mustUpdate(t, fc, "default", "test-ingress", func(ing *networkingv1.Ingress) {
		ing.Spec.TLS[0].Hosts[0] = "updated-svc"
	})
	expectReconciled(t, ingPGR, "default", "test-ingress")
	populateTLSSecret(context.Background(), fc, "test-pg", "updated-svc.ts.net")
	expectReconciled(t, ingPGR, "default", "test-ingress")
	verifyServeConfig(t, fc, "svc:updated-svc", false)
	verifyTailscaleService(t, ft, "svc:updated-svc", []string{"tcp:443"})
	verifyTailscaledConfig(t, fc, []string{"svc:updated-svc"})

	_, err := ft.GetVIPService(context.Background(), tailcfg.ServiceName("svc:my-svc"))
	if err == nil {
		t.Fatalf("svc:my-svc not cleaned up")
	}
	if !isErrorTailscaleServiceNotFound(err) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateIngress(t *testing.T) {
	baseIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationProxyGroup: "test-pg",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("tailscale"),
			TLS: []networkingv1.IngressTLS{
				{Hosts: []string{"test"}},
			},
		},
	}

	readyProxyGroup := &tsapi.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-pg",
			Generation: 1,
		},
		Spec: tsapi.ProxyGroupSpec{
			Type: tsapi.ProxyGroupTypeIngress,
		},
		Status: tsapi.ProxyGroupStatus{
			Conditions: []metav1.Condition{
				{
					Type:               string(tsapi.ProxyGroupReady),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
				},
			},
		},
	}

	tests := []struct {
		name         string
		ing          *networkingv1.Ingress
		pg           *tsapi.ProxyGroup
		existingIngs []networkingv1.Ingress
		wantErr      string
	}{
		{
			name: "valid_ingress_with_hostname",
			ing: &networkingv1.Ingress{
				ObjectMeta: baseIngress.ObjectMeta,
				Spec: networkingv1.IngressSpec{
					TLS: []networkingv1.IngressTLS{
						{Hosts: []string{"test.example.com"}},
					},
				},
			},
			pg: readyProxyGroup,
		},
		{
			name: "valid_ingress_with_default_hostname",
			ing:  baseIngress,
			pg:   readyProxyGroup,
		},
		{
			name: "invalid_tags",
			ing: &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name:      baseIngress.Name,
					Namespace: baseIngress.Namespace,
					Annotations: map[string]string{
						AnnotationTags: "tag:invalid!",
					},
				},
			},
			pg:      readyProxyGroup,
			wantErr: "tailscale.com/tags annotation contains invalid tag \"tag:invalid!\": tag names can only contain numbers, letters, or dashes",
		},
		{
			name: "multiple_TLS_entries",
			ing: &networkingv1.Ingress{
				ObjectMeta: baseIngress.ObjectMeta,
				Spec: networkingv1.IngressSpec{
					TLS: []networkingv1.IngressTLS{
						{Hosts: []string{"test1.example.com"}},
						{Hosts: []string{"test2.example.com"}},
					},
				},
			},
			pg:      readyProxyGroup,
			wantErr: "Ingress contains invalid TLS block [{[test1.example.com] } {[test2.example.com] }]: only a single TLS entry with a single host is allowed",
		},
		{
			name: "multiple_hosts_in_TLS_entry",
			ing: &networkingv1.Ingress{
				ObjectMeta: baseIngress.ObjectMeta,
				Spec: networkingv1.IngressSpec{
					TLS: []networkingv1.IngressTLS{
						{Hosts: []string{"test1.example.com", "test2.example.com"}},
					},
				},
			},
			pg:      readyProxyGroup,
			wantErr: "Ingress contains invalid TLS block [{[test1.example.com test2.example.com] }]: only a single TLS entry with a single host is allowed",
		},
		{
			name: "wrong_proxy_group_type",
			ing:  baseIngress,
			pg: &tsapi.ProxyGroup{
				ObjectMeta: readyProxyGroup.ObjectMeta,
				Spec: tsapi.ProxyGroupSpec{
					Type: tsapi.ProxyGroupType("foo"),
				},
				Status: readyProxyGroup.Status,
			},
			wantErr: "ProxyGroup \"test-pg\" is of type \"foo\" but must be of type \"ingress\"",
		},
		{
			name: "proxy_group_not_ready",
			ing:  baseIngress,
			pg: &tsapi.ProxyGroup{
				ObjectMeta: readyProxyGroup.ObjectMeta,
				Spec:       readyProxyGroup.Spec,
				Status: tsapi.ProxyGroupStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(tsapi.ProxyGroupReady),
							Status:             metav1.ConditionFalse,
							ObservedGeneration: 1,
						},
					},
				},
			},
			wantErr: "ProxyGroup \"test-pg\" is not ready",
		},
		{
			name: "duplicate_hostname",
			ing:  baseIngress,
			pg:   readyProxyGroup,
			existingIngs: []networkingv1.Ingress{{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "existing-ingress",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationProxyGroup: "test-pg",
					},
				},
				Spec: networkingv1.IngressSpec{
					IngressClassName: ptr.To("tailscale"),
					TLS: []networkingv1.IngressTLS{
						{Hosts: []string{"test"}},
					},
				},
			}},
			wantErr: `found duplicate Ingress "existing-ingress" for hostname "test" - multiple Ingresses for the same hostname in the same cluster are not allowed`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().
				WithScheme(tsapi.GlobalScheme).
				WithObjects(tt.ing).
				WithLists(&networkingv1.IngressList{Items: tt.existingIngs}).
				Build()
			r := &HAIngressReconciler{Client: fc}
			err := r.validateIngress(context.Background(), tt.ing, tt.pg)
			if (err == nil && tt.wantErr != "") || (err != nil && err.Error() != tt.wantErr) {
				t.Errorf("validateIngress() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIngressPGReconciler_HTTPEndpoint(t *testing.T) {
	ingPGR, fc, ft := setupIngressTest(t)

	// Create test Ingress with HTTP endpoint enabled
	ing := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{Kind: "Ingress", APIVersion: "networking.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
			UID:       types.UID("1234-UID"),
			Annotations: map[string]string{
				"tailscale.com/proxy-group":   "test-pg",
				"tailscale.com/http-endpoint": "enabled",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("tailscale"),
			DefaultBackend: &networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{
					Name: "test",
					Port: networkingv1.ServiceBackendPort{
						Number: 8080,
					},
				},
			},
			TLS: []networkingv1.IngressTLS{
				{Hosts: []string{"my-svc"}},
			},
		},
	}
	if err := fc.Create(context.Background(), ing); err != nil {
		t.Fatal(err)
	}

	// Verify initial reconciliation with HTTP enabled
	expectReconciled(t, ingPGR, "default", "test-ingress")
	populateTLSSecret(context.Background(), fc, "test-pg", "my-svc.ts.net")
	expectReconciled(t, ingPGR, "default", "test-ingress")
	verifyTailscaleService(t, ft, "svc:my-svc", []string{"tcp:80", "tcp:443"})
	verifyServeConfig(t, fc, "svc:my-svc", true)

	// Verify Ingress status
	ing = &networkingv1.Ingress{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      "test-ingress",
		Namespace: "default",
	}, ing); err != nil {
		t.Fatal(err)
	}

	// Status will be empty until the Tailscale Service shows up in prefs.
	if !reflect.DeepEqual(ing.Status.LoadBalancer.Ingress, []networkingv1.IngressLoadBalancerIngress(nil)) {
		t.Errorf("incorrect Ingress status: got %v, want empty",
			ing.Status.LoadBalancer.Ingress)
	}

	// Add the Tailscale Service to prefs to have the Ingress recognised as ready.
	mustCreate(t, fc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pg-0",
			Namespace: "operator-ns",
			Labels:    pgSecretLabels("test-pg", "state"),
		},
		Data: map[string][]byte{
			"_current-profile": []byte("profile-foo"),
			"profile-foo":      []byte(`{"AdvertiseServices":["svc:my-svc"],"Config":{"NodeID":"node-foo"}}`),
		},
	})

	// Reconcile and re-fetch Ingress.
	expectReconciled(t, ingPGR, "default", "test-ingress")
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(ing), ing); err != nil {
		t.Fatal(err)
	}

	wantStatus := []networkingv1.IngressPortStatus{
		{Port: 443, Protocol: "TCP"},
		{Port: 80, Protocol: "TCP"},
	}
	if !reflect.DeepEqual(ing.Status.LoadBalancer.Ingress[0].Ports, wantStatus) {
		t.Errorf("incorrect status ports: got %v, want %v",
			ing.Status.LoadBalancer.Ingress[0].Ports, wantStatus)
	}

	// Remove HTTP endpoint annotation
	mustUpdate(t, fc, "default", "test-ingress", func(ing *networkingv1.Ingress) {
		delete(ing.Annotations, "tailscale.com/http-endpoint")
	})

	// Verify reconciliation after removing HTTP
	expectReconciled(t, ingPGR, "default", "test-ingress")
	verifyTailscaleService(t, ft, "svc:my-svc", []string{"tcp:443"})
	verifyServeConfig(t, fc, "svc:my-svc", false)

	// Verify Ingress status
	ing = &networkingv1.Ingress{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      "test-ingress",
		Namespace: "default",
	}, ing); err != nil {
		t.Fatal(err)
	}

	wantStatus = []networkingv1.IngressPortStatus{
		{Port: 443, Protocol: "TCP"},
	}
	if !reflect.DeepEqual(ing.Status.LoadBalancer.Ingress[0].Ports, wantStatus) {
		t.Errorf("incorrect status ports: got %v, want %v",
			ing.Status.LoadBalancer.Ingress[0].Ports, wantStatus)
	}
}

func verifyTailscaleService(t *testing.T, ft *fakeTSClient, serviceName string, wantPorts []string) {
	t.Helper()
	tsSvc, err := ft.GetVIPService(context.Background(), tailcfg.ServiceName(serviceName))
	if err != nil {
		t.Fatalf("getting Tailscale Service %q: %v", serviceName, err)
	}
	if tsSvc == nil {
		t.Fatalf("Tailscale Service %q not created", serviceName)
	}
	gotPorts := slices.Clone(tsSvc.Ports)
	slices.Sort(gotPorts)
	slices.Sort(wantPorts)
	if !slices.Equal(gotPorts, wantPorts) {
		t.Errorf("incorrect ports for Tailscale Service %q: got %v, want %v", serviceName, gotPorts, wantPorts)
	}
}

func verifyServeConfig(t *testing.T, fc client.Client, serviceName string, wantHTTP bool) {
	t.Helper()

	cm := &corev1.ConfigMap{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      "test-pg-ingress-config",
		Namespace: "operator-ns",
	}, cm); err != nil {
		t.Fatalf("getting ConfigMap: %v", err)
	}

	cfg := &ipn.ServeConfig{}
	if err := json.Unmarshal(cm.BinaryData["serve-config.json"], cfg); err != nil {
		t.Fatalf("unmarshaling serve config: %v", err)
	}

	t.Logf("Looking for service %q in config: %+v", serviceName, cfg)

	svc := cfg.Services[tailcfg.ServiceName(serviceName)]
	if svc == nil {
		t.Fatalf("service %q not found in serve config, services: %+v", serviceName, maps.Keys(cfg.Services))
	}

	wantHandlers := 1
	if wantHTTP {
		wantHandlers = 2
	}

	// Check TCP handlers
	if len(svc.TCP) != wantHandlers {
		t.Errorf("incorrect number of TCP handlers for service %q: got %d, want %d", serviceName, len(svc.TCP), wantHandlers)
	}
	if wantHTTP {
		if h, ok := svc.TCP[uint16(80)]; !ok {
			t.Errorf("HTTP (port 80) handler not found for service %q", serviceName)
		} else if !h.HTTP {
			t.Errorf("HTTP not enabled for port 80 handler for service %q", serviceName)
		}
	}
	if h, ok := svc.TCP[uint16(443)]; !ok {
		t.Errorf("HTTPS (port 443) handler not found for service %q", serviceName)
	} else if !h.HTTPS {
		t.Errorf("HTTPS not enabled for port 443 handler for service %q", serviceName)
	}

	// Check Web handlers
	if len(svc.Web) != wantHandlers {
		t.Errorf("incorrect number of Web handlers for service %q: got %d, want %d", serviceName, len(svc.Web), wantHandlers)
	}
}

func verifyTailscaledConfig(t *testing.T, fc client.Client, expectedServices []string) {
	t.Helper()
	var expected string
	if expectedServices != nil && len(expectedServices) > 0 {
		expectedServicesJSON, err := json.Marshal(expectedServices)
		if err != nil {
			t.Fatalf("marshaling expected services: %v", err)
		}
		expected = fmt.Sprintf(`,"AdvertiseServices":%s`, expectedServicesJSON)
	}
	expectEqual(t, fc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pgConfigSecretName("test-pg", 0),
			Namespace: "operator-ns",
			Labels:    pgSecretLabels("test-pg", "config"),
		},
		Data: map[string][]byte{
			tsoperator.TailscaledConfigFileName(106): []byte(fmt.Sprintf(`{"Version":""%s}`, expected)),
		},
	})
}

func setupIngressTest(t *testing.T) (*HAIngressReconciler, client.Client, *fakeTSClient) {
	tsIngressClass := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tailscale"},
		Spec:       networkingv1.IngressClassSpec{Controller: "tailscale.com/ts-ingress"},
	}

	// Pre-create the ProxyGroup
	pg := &tsapi.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-pg",
			Generation: 1,
		},
		Spec: tsapi.ProxyGroupSpec{
			Type: tsapi.ProxyGroupTypeIngress,
		},
	}

	// Pre-create the ConfigMap for the ProxyGroup
	pgConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pg-ingress-config",
			Namespace: "operator-ns",
		},
		BinaryData: map[string][]byte{
			"serve-config.json": []byte(`{"Services":{}}`),
		},
	}

	// Pre-create a config Secret for the ProxyGroup
	pgCfgSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pgConfigSecretName("test-pg", 0),
			Namespace: "operator-ns",
			Labels:    pgSecretLabels("test-pg", "config"),
		},
		Data: map[string][]byte{
			tsoperator.TailscaledConfigFileName(106): []byte("{}"),
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(tsapi.GlobalScheme).
		WithObjects(pg, pgCfgSecret, pgConfigMap, tsIngressClass).
		WithStatusSubresource(pg).
		Build()

	// Set ProxyGroup status to ready
	pg.Status.Conditions = []metav1.Condition{
		{
			Type:               string(tsapi.ProxyGroupReady),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
		},
	}
	if err := fc.Status().Update(context.Background(), pg); err != nil {
		t.Fatal(err)
	}
	fakeTsnetServer := &fakeTSNetServer{certDomains: []string{"foo.com"}}

	ft := &fakeTSClient{}
	zl, err := zap.NewDevelopment()
	if err != nil {
		t.Fatal(err)
	}

	lc := &fakeLocalClient{
		status: &ipnstate.Status{
			CurrentTailnet: &ipnstate.TailnetStatus{
				MagicDNSSuffix: "ts.net",
			},
		},
	}

	ingPGR := &HAIngressReconciler{
		Client:      fc,
		tsClient:    ft,
		defaultTags: []string{"tag:k8s"},
		tsNamespace: "operator-ns",
		tsnetServer: fakeTsnetServer,
		logger:      zl.Sugar(),
		recorder:    record.NewFakeRecorder(10),
		lc:          lc,
	}

	return ingPGR, fc, ft
}

func TestIngressPGReconciler_MultiCluster(t *testing.T) {
	ingPGR, fc, ft := setupIngressTest(t)
	ingPGR.operatorID = "operator-1"

	// Create initial Ingress
	ing := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{Kind: "Ingress", APIVersion: "networking.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
			UID:       types.UID("1234-UID"),
			Annotations: map[string]string{
				"tailscale.com/proxy-group": "test-pg",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("tailscale"),
			TLS: []networkingv1.IngressTLS{
				{Hosts: []string{"my-svc"}},
			},
		},
	}
	mustCreate(t, fc, ing)

	// Simulate existing Tailscale Service from another cluster
	existingVIPSvc := &tailscale.VIPService{
		Name: "svc:my-svc",
		Annotations: map[string]string{
			ownerAnnotation: `{"ownerrefs":[{"operatorID":"operator-2"}]}`,
		},
	}
	ft.vipServices = map[tailcfg.ServiceName]*tailscale.VIPService{
		"svc:my-svc": existingVIPSvc,
	}

	// Verify reconciliation adds our operator reference
	expectReconciled(t, ingPGR, "default", "test-ingress")

	tsSvc, err := ft.GetVIPService(context.Background(), "svc:my-svc")
	if err != nil {
		t.Fatalf("getting Tailscale Service: %v", err)
	}
	if tsSvc == nil {
		t.Fatal("Tailscale Service not found")
	}

	o, err := parseOwnerAnnotation(tsSvc)
	if err != nil {
		t.Fatalf("parsing owner annotation: %v", err)
	}

	wantOwnerRefs := []OwnerRef{
		{OperatorID: "operator-2"},
		{OperatorID: "operator-1"},
	}
	if !reflect.DeepEqual(o.OwnerRefs, wantOwnerRefs) {
		t.Errorf("incorrect owner refs\ngot:  %+v\nwant: %+v", o.OwnerRefs, wantOwnerRefs)
	}

	// Delete the Ingress and verify Tailscale Service still exists with one owner ref
	if err := fc.Delete(context.Background(), ing); err != nil {
		t.Fatalf("deleting Ingress: %v", err)
	}
	expectRequeue(t, ingPGR, "default", "test-ingress")

	tsSvc, err = ft.GetVIPService(context.Background(), "svc:my-svc")
	if err != nil {
		t.Fatalf("getting Tailscale Service after deletion: %v", err)
	}
	if tsSvc == nil {
		t.Fatal("Tailscale Service was incorrectly deleted")
	}

	o, err = parseOwnerAnnotation(tsSvc)
	if err != nil {
		t.Fatalf("parsing owner annotation: %v", err)
	}

	wantOwnerRefs = []OwnerRef{
		{OperatorID: "operator-2"},
	}
	if !reflect.DeepEqual(o.OwnerRefs, wantOwnerRefs) {
		t.Errorf("incorrect owner refs after deletion\ngot:  %+v\nwant: %+v", o.OwnerRefs, wantOwnerRefs)
	}
}

func populateTLSSecret(ctx context.Context, c client.Client, pgName, domain string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      domain,
			Namespace: "operator-ns",
			Labels: map[string]string{
				kubetypes.LabelManaged:    "true",
				labelProxyGroup:           pgName,
				labelDomain:               domain,
				kubetypes.LabelSecretType: "certs",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("fake-cert"),
			corev1.TLSPrivateKeyKey: []byte("fake-key"),
		},
	}

	_, err := createOrUpdate(ctx, c, "operator-ns", secret, func(s *corev1.Secret) {
		s.Data = secret.Data
	})
	return err
}
