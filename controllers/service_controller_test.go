/*
Copyright 2019-2023 Google LLC.

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

package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// stubBackendController is a no-op BackendController for Reconcile tests that
// don't care about actual GCLB backend reconciliation.
type stubBackendController struct{}

func (stubBackendController) ReconcileBackends(_ context.Context, _ AutonegStatus, _ AutonegStatus, _ bool) error {
	return nil
}

type capturedInfoLog struct {
	msg string
	kvs []interface{}
}

// capturingLogSink records every Info() call so tests can assert on the
// exact values passed to the logger, rather than on rendered text.
type capturingLogSink struct {
	logs *[]capturedInfoLog
}

func (s capturingLogSink) Init(_ logr.RuntimeInfo)                   {}
func (s capturingLogSink) Enabled(_ int) bool                        { return true }
func (s capturingLogSink) Error(_ error, _ string, _ ...interface{}) {}
func (s capturingLogSink) WithValues(_ ...interface{}) logr.LogSink  { return s }
func (s capturingLogSink) WithName(_ string) logr.LogSink            { return s }
func (s capturingLogSink) Info(_ int, msg string, keysAndValues ...interface{}) {
	*s.logs = append(*s.logs, capturedInfoLog{msg: msg, kvs: keysAndValues})
}

// TestReconcileLogsExistingStatusWithoutPointerLeak pins the fix for the
// "Existing status" log line: it must log each part of the internal Statuses
// wrapper (config, negConfig, status, negStatus, syncConfig) as its own
// structured field. Before the fix, it logged fmt.Sprintf("%+v", status)
// where status.status.BackendServices[...].CapacityScaler/InitialCapacity
// are *StringOrInt - fmt prints such pointers as raw memory addresses (e.g.
// 0xc0007aad78) instead of dereferencing them, making the log line useless.
func TestReconcileLogsExistingStatusWithoutPointerLeak(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:      "old-service",
			Namespace: "ns",
			Annotations: map[string]string{
				// Populates status.config, exercising the same
				// *StringOrInt pointer fields as the status annotation below.
				autonegAnnotation: `{"backend_services":{"80":[` +
					`{"name":"http-be-current","initial_capacity":3,"capacity_scaler":7}]}}`,
				// Populates status.negConfig.
				negAnnotation: `{"exposed_ports":{"80":{}}}`,
				// Populates status.negStatus.
				negStatusAnnotation: `{"network_endpoint_groups":{"80":"k8s1-abc123"},"zones":["europe-west1-b"]}`,
				// Populates status.status, the previously recorded status.
				autonegStatusAnnotation: `{"backend_services":{"80":{"http-be":` +
					`{"name":"http-be","initial_capacity":10,"capacity_scaler":42}}}}`,
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(svc).Build()

	var logs []capturedInfoLog
	ctx := logf.IntoContext(context.Background(), logr.New(capturingLogSink{logs: &logs}))

	r := &ServiceReconciler{
		Client:                            fakeClient,
		BackendController:                 stubBackendController{},
		Recorder:                          record.NewFakeRecorder(10),
		ServiceNameTemplate:               "{namespace}-{name}-{port}-{hash}",
		AllowServiceName:                  true,
		DeregisterNEGsOnAnnotationRemoval: true,
		ErrorCount:                        map[string]int{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "old-service", Namespace: "ns"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var found bool
	for _, l := range logs {
		if l.msg != "Existing status" {
			continue
		}
		found = true

		if len(l.kvs)%2 != 0 {
			t.Fatalf(`expected "Existing status" log to carry key/value pairs, got: %#v`, l.kvs)
		}
		fields := map[string]interface{}{}
		for i := 0; i < len(l.kvs); i += 2 {
			key, ok := l.kvs[i].(string)
			if !ok {
				t.Fatalf(`expected string keys in "Existing status" log, got: %#v`, l.kvs[i])
			}
			fields[key] = l.kvs[i+1]
		}

		// Every part of the internal Statuses wrapper should be logged
		// individually, matching what the old (buggy) dump exposed.
		marshaled := map[string]string{}
		for _, key := range []string{"config", "negConfig", "status", "negStatus", "syncConfig"} {
			v, ok := fields[key]
			if !ok {
				t.Fatalf(`expected "Existing status" log to include a %q field, got: %#v`, key, fields)
			}
			if _, isString := v.(string); isString {
				t.Fatalf(`%q field must be a structured value, not a pre-formatted string, got: %v`, key, v)
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf(`%q field must marshal to JSON cleanly, got error: %v`, key, err)
			}
			if strings.Contains(string(b), "0x") {
				t.Fatalf(`%q field must not contain a raw pointer address, got: %s`, key, b)
			}
			marshaled[key] = string(b)
		}

		// Each field should carry the real, annotation-derived data, not
		// just an empty/zero value - otherwise the "no 0x" check above
		// would trivially pass without exercising the pointer fields.
		if !strings.Contains(marshaled["config"], `"capacity_scaler":7`) || !strings.Contains(marshaled["config"], `"initial_capacity":3`) {
			t.Fatalf(`expected "config" field to contain real capacity_scaler/initial_capacity values, got: %s`, marshaled["config"])
		}
		if !strings.Contains(marshaled["status"], `"capacity_scaler":42`) || !strings.Contains(marshaled["status"], `"initial_capacity":10`) {
			t.Fatalf(`expected "status" field to contain real capacity_scaler/initial_capacity values, got: %s`, marshaled["status"])
		}
		if !strings.Contains(marshaled["negStatus"], "k8s1-abc123") {
			t.Fatalf(`expected "negStatus" field to contain the real NEG name, got: %s`, marshaled["negStatus"])
		}
		if !strings.Contains(marshaled["negConfig"], `"80"`) {
			t.Fatalf(`expected "negConfig" field to contain the exposed port, got: %s`, marshaled["negConfig"])
		}
	}
	if !found {
		t.Fatalf(`expected an "Existing status" log entry, got none`)
	}
}
