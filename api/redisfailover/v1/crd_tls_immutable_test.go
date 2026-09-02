package v1

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// The generated CRD is committed in three places; the operator's manifest is
// the one the generator writes, the others are copies.
var generatedCRDPaths = []string{
	"../../../manifests/databases.spotahome.com_redisfailovers.yaml",
	"../../../manifests/kustomize/base/databases.spotahome.com_redisfailovers.yaml",
	"../../../charts/redisoperator/crds/databases.spotahome.com_redisfailovers.yaml",
}

// Limits the apiserver applies per CEL expression and per object; the
// values of PerCallLimit and RuntimeCELCostBudget in
// k8s.io/apiserver/pkg/apis/cel.
const (
	celPerCallLimit  = 1_000_000
	celPerObjectCost = 10_000_000
)

const tlsImmutableMessage = "TLS cannot be enabled or disabled on an existing RedisFailover; create a new one"

// requireGeneratedCRDs skips when the tree carries none of the generated CRD
// copies. Consumers that vendor this operator as a source-only patch take the
// Go tree and drop manifests/ and charts/, so the files these tests read are
// not there at all and `go test ./...` would fail for want of a fixture rather
// than for anything about the operator. A tree holding some of the copies but
// not the others is a different matter: they are written together and are
// expected to travel together, so a partial set stays a failure.
func requireGeneratedCRDs(t *testing.T) {
	t.Helper()
	var missing []string
	for _, path := range generatedCRDPaths {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, path)
		}
	}
	if len(missing) == len(generatedCRDPaths) {
		t.Skipf("no generated CRD in this tree (absent: %s); nothing to validate when the operator is vendored as source only",
			strings.Join(missing, ", "))
	}
}

// generatedCRDValidator compiles the x-kubernetes-validations of the
// committed CRD, so the assertions below run against the schema the
// apiserver enforces rather than against a copy of the rule.
func generatedCRDValidator(t *testing.T) *schemacel.Validator {
	t.Helper()
	raw, err := os.ReadFile(generatedCRDPaths[0])
	if err != nil {
		t.Fatalf("reading the generated CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("decoding the generated CRD: %v", err)
	}
	var openAPI *apiextensionsv1.JSONSchemaProps
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1" && version.Schema != nil {
			openAPI = version.Schema.OpenAPIV3Schema
		}
	}
	if openAPI == nil {
		t.Fatal("the generated CRD carries no v1 schema")
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(openAPI, &internal, nil); err != nil {
		t.Fatalf("converting the schema: %v", err)
	}
	structural, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatalf("building the structural schema: %v", err)
	}
	validator := schemacel.NewValidator(structural, true, celPerCallLimit)
	if validator == nil {
		t.Fatal("the generated CRD carries no x-kubernetes-validations rules")
	}
	return validator
}

// redisFailoverObject is a RedisFailover as the apiserver sees it. A nil
// tls leaves spec.tls absent, which is how a plaintext failover is written.
func redisFailoverObject(tls map[string]interface{}) map[string]interface{} {
	spec := map[string]interface{}{
		"redis":    map[string]interface{}{"replicas": int64(3)},
		"sentinel": map[string]interface{}{"replicas": int64(3)},
	}
	if tls != nil {
		spec["tls"] = tls
	}
	return map[string]interface{}{
		"apiVersion": "databases.spotahome.com/v1",
		"kind":       "RedisFailover",
		"metadata":   map[string]interface{}{"name": "rf", "namespace": "default"},
		"spec":       spec,
	}
}

func tlsBlock(enabled bool, extraSANs ...string) map[string]interface{} {
	certManager := map[string]interface{}{
		"issuerRef": map[string]interface{}{"name": "issuer"},
	}
	if len(extraSANs) > 0 {
		sans := make([]interface{}, 0, len(extraSANs))
		for _, san := range extraSANs {
			sans = append(sans, san)
		}
		certManager["extraSANs"] = sans
	}
	block := map[string]interface{}{"certManager": certManager}
	if enabled {
		block["enabled"] = true
	}
	return block
}

func validateTransition(t *testing.T, validator *schemacel.Validator, obj, oldObj map[string]interface{}) field.ErrorList {
	t.Helper()
	var old interface{}
	if oldObj != nil {
		old = oldObj
	}
	errs, _ := validator.Validate(context.Background(), nil, nil, obj, old, celPerObjectCost)
	return errs
}

func assertTLSImmutableError(t *testing.T, errs field.ErrorList) {
	t.Helper()
	if !assert.Len(t, errs, 1) {
		return
	}
	assert.Equal(t, "spec", errs[0].Field)
	assert.Contains(t, errs[0].Detail, tlsImmutableMessage)
}

func TestGeneratedCRDRejectsTurningTLSOnOrOff(t *testing.T) {
	requireGeneratedCRDs(t)
	validator := generatedCRDValidator(t)

	t.Run("create with tls is accepted", func(t *testing.T) {
		assert.Empty(t, validateTransition(t, validator, redisFailoverObject(tlsBlock(true)), nil))
	})

	t.Run("create without tls is accepted", func(t *testing.T) {
		assert.Empty(t, validateTransition(t, validator, redisFailoverObject(nil), nil))
	})

	t.Run("absent to enabled is rejected", func(t *testing.T) {
		assertTLSImmutableError(t, validateTransition(t, validator, redisFailoverObject(tlsBlock(true)), redisFailoverObject(nil)))
	})

	t.Run("enabled to absent is rejected", func(t *testing.T) {
		assertTLSImmutableError(t, validateTransition(t, validator, redisFailoverObject(nil), redisFailoverObject(tlsBlock(true))))
	})

	t.Run("enabled to disabled in place is rejected", func(t *testing.T) {
		assertTLSImmutableError(t, validateTransition(t, validator, redisFailoverObject(tlsBlock(false)), redisFailoverObject(tlsBlock(true))))
	})

	t.Run("disabled to enabled in place is rejected", func(t *testing.T) {
		assertTLSImmutableError(t, validateTransition(t, validator, redisFailoverObject(tlsBlock(true)), redisFailoverObject(tlsBlock(false))))
	})

	t.Run("adding a disabled block to a plaintext failover is accepted", func(t *testing.T) {
		assert.Empty(t, validateTransition(t, validator, redisFailoverObject(tlsBlock(false)), redisFailoverObject(nil)))
	})

	t.Run("changing extraSANs with tls on is accepted", func(t *testing.T) {
		assert.Empty(t, validateTransition(t, validator, redisFailoverObject(tlsBlock(true, "redis.example.com")), redisFailoverObject(tlsBlock(true))))
	})

	t.Run("changing authClients with tls on is accepted", func(t *testing.T) {
		updated := tlsBlock(true)
		updated["authClients"] = "yes"
		assert.Empty(t, validateTransition(t, validator, redisFailoverObject(updated), redisFailoverObject(tlsBlock(true))))
	})
}

// The generator writes one file and the other two are copies; a copy that
// falls behind ships a different schema to chart and kustomize users.
func TestGeneratedCRDCopiesAreIdentical(t *testing.T) {
	requireGeneratedCRDs(t)
	reference, err := os.ReadFile(generatedCRDPaths[0])
	if err != nil {
		t.Fatalf("reading %s: %v", generatedCRDPaths[0], err)
	}
	for _, path := range generatedCRDPaths[1:] {
		copyContent, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		assert.True(t, bytes.Equal(reference, copyContent), "%s differs from %s", path, generatedCRDPaths[0])
	}
}
