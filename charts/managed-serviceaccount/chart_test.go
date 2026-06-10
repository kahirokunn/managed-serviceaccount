package chart_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

const (
	metricsServiceTemplate    = "managed-serviceaccount/templates/manager-metrics-service.yaml"
	metricsMonitorTemplate    = "managed-serviceaccount/templates/manager-servicemonitor.yaml"
	managerDeploymentTemplate = "managed-serviceaccount/templates/manager-deployment.yaml"
	addOnTemplateTemplate     = "managed-serviceaccount/templates/addontemplate.yaml"
)

func chartDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to determine chart test file location")
	}
	return filepath.Dir(filename)
}

func renderChart(t *testing.T, values map[string]interface{}) map[string]string {
	t.Helper()
	c, err := loader.Load(chartDir(t))
	if err != nil {
		t.Fatalf("load chart: %v", err)
	}
	vals, err := chartutil.ToRenderValues(c, values,
		chartutil.ReleaseOptions{Name: "msa", Namespace: "open-cluster-management-addon", IsInstall: true},
		chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatalf("compose render values: %v", err)
	}
	out, err := engine.Render(c, vals)
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	return out
}

func rendered(out map[string]string, key string) string {
	return strings.TrimSpace(out[key])
}

func TestManagerMetricsRenderGates(t *testing.T) {
	cases := []struct {
		name                 string
		values               map[string]interface{}
		wantService          bool
		wantServiceMonitor   bool
		wantManagerDeploy    bool
		wantAddOnTemplate    bool
		extraServiceChecks   []string
		extraMonitorChecks   []string
		extraDeployArgsCheck []string
	}{
		{
			name:               "deployment mode defaults exposes Service but not ServiceMonitor",
			values:             map[string]interface{}{},
			wantService:        true,
			wantServiceMonitor: false,
			wantManagerDeploy:  true,
			wantAddOnTemplate:  false,
			extraServiceChecks: []string{
				"name: managed-serviceaccount-addon-manager-metrics",
				"port: 38080",
				"open-cluster-management.io/addon: managed-serviceaccount",
			},
			extraDeployArgsCheck: []string{
				"--metrics-bind-address=:38080",
				"containerPort: 38080",
			},
		},
		{
			name: "deployment mode with serviceMonitor enabled renders both",
			values: map[string]interface{}{
				"metrics": map[string]interface{}{
					"serviceMonitor": map[string]interface{}{
						"enabled": true,
						"labels": map[string]interface{}{
							"release": "kube-prometheus",
						},
					},
				},
			},
			wantService:        true,
			wantServiceMonitor: true,
			wantManagerDeploy:  true,
			wantAddOnTemplate:  false,
			extraMonitorChecks: []string{
				"kind: ServiceMonitor",
				"release: kube-prometheus",
				"open-cluster-management.io/addon: managed-serviceaccount",
				"port: metrics",
			},
		},
		{
			name: "deployment mode with metrics disabled skips Service and ServiceMonitor",
			values: map[string]interface{}{
				"metrics": map[string]interface{}{
					"enabled": false,
					"serviceMonitor": map[string]interface{}{
						"enabled": true,
					},
				},
			},
			wantService:        false,
			wantServiceMonitor: false,
			wantManagerDeploy:  true,
			wantAddOnTemplate:  false,
		},
		{
			name: "AddOnTemplate mode without clusterProfile skips manager Service and ServiceMonitor",
			values: map[string]interface{}{
				"hubDeployMode": "AddOnTemplate",
				"metrics": map[string]interface{}{
					"serviceMonitor": map[string]interface{}{
						"enabled": true,
					},
				},
			},
			wantService:        false,
			wantServiceMonitor: false,
			wantManagerDeploy:  false,
			wantAddOnTemplate:  true,
		},
		{
			name: "AddOnTemplate mode with clusterProfile feature gate exposes Service and ServiceMonitor",
			values: map[string]interface{}{
				"hubDeployMode": "AddOnTemplate",
				"featureGates": map[string]interface{}{
					"clusterProfile": true,
				},
				"metrics": map[string]interface{}{
					"serviceMonitor": map[string]interface{}{
						"enabled": true,
					},
				},
			},
			wantService:        true,
			wantServiceMonitor: true,
			wantManagerDeploy:  true,
			wantAddOnTemplate:  true,
		},
		{
			name: "custom metrics port propagates to Service and Deployment",
			values: map[string]interface{}{
				"metrics": map[string]interface{}{
					"port": 19090,
				},
			},
			wantService:       true,
			wantManagerDeploy: true,
			extraServiceChecks: []string{
				"port: 19090",
				"targetPort: 19090",
			},
			extraDeployArgsCheck: []string{
				"--metrics-bind-address=:19090",
				"containerPort: 19090",
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out := renderChart(t, tc.values)

			svc := rendered(out, metricsServiceTemplate)
			if tc.wantService && svc == "" {
				t.Fatalf("expected manager metrics Service to render, got empty output")
			}
			if !tc.wantService && svc != "" {
				t.Fatalf("expected manager metrics Service to be gated off, got:\n%s", svc)
			}
			for _, want := range tc.extraServiceChecks {
				if !strings.Contains(svc, want) {
					t.Errorf("Service render missing %q. Full output:\n%s", want, svc)
				}
			}

			sm := rendered(out, metricsMonitorTemplate)
			if tc.wantServiceMonitor && sm == "" {
				t.Fatalf("expected ServiceMonitor to render, got empty output")
			}
			if !tc.wantServiceMonitor && sm != "" {
				t.Fatalf("expected ServiceMonitor to be gated off, got:\n%s", sm)
			}
			for _, want := range tc.extraMonitorChecks {
				if !strings.Contains(sm, want) {
					t.Errorf("ServiceMonitor render missing %q. Full output:\n%s", want, sm)
				}
			}

			deploy := rendered(out, managerDeploymentTemplate)
			if tc.wantManagerDeploy && deploy == "" {
				t.Fatalf("expected manager Deployment to render, got empty output")
			}
			if !tc.wantManagerDeploy && deploy != "" {
				t.Fatalf("expected manager Deployment to be gated off, got:\n%s", deploy)
			}
			for _, want := range tc.extraDeployArgsCheck {
				if !strings.Contains(deploy, want) {
					t.Errorf("Deployment render missing %q. Full output:\n%s", want, deploy)
				}
			}

			addonTmpl := rendered(out, addOnTemplateTemplate)
			if tc.wantAddOnTemplate && addonTmpl == "" {
				t.Fatalf("expected AddOnTemplate to render, got empty output")
			}
			if !tc.wantAddOnTemplate && addonTmpl != "" {
				t.Fatalf("expected AddOnTemplate to be gated off, got:\n%s", addonTmpl)
			}
		})
	}
}
