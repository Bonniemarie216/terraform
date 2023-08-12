// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package s3

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/aws-sdk-go-base/v2/mockdata"
	"github.com/hashicorp/aws-sdk-go-base/v2/servicemocks"
	"github.com/hashicorp/terraform/internal/backend"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/configs/hcl2shim"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/remote"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/exp/maps"
)

var (
	mockStsGetCallerIdentityRequestBody = url.Values{
		"Action":  []string{"GetCallerIdentity"},
		"Version": []string{"2011-06-15"},
	}.Encode()
)

// verify that we are doing ACC tests or the S3 tests specifically
func testACC(t *testing.T) {
	skip := os.Getenv("TF_ACC") == "" && os.Getenv("TF_S3_TEST") == ""
	if skip {
		t.Log("s3 backend tests require setting TF_ACC or TF_S3_TEST")
		t.Skip()
	}
	if os.Getenv("AWS_DEFAULT_REGION") == "" {
		os.Setenv("AWS_DEFAULT_REGION", "us-west-2")
	}
}

func TestBackend_impl(t *testing.T) {
	var _ backend.Backend = new(Backend)
}

func TestBackendConfig_original(t *testing.T) {
	testACC(t)

	ctx := context.TODO()

	config := map[string]interface{}{
		"region":         "us-west-1",
		"bucket":         "tf-test",
		"key":            "state",
		"encrypt":        true,
		"dynamodb_table": "dynamoTable",
	}

	b := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(config)).(*Backend)

	if b.awsConfig.Region != "us-west-1" {
		t.Fatalf("Incorrect region was populated")
	}
	if b.awsConfig.RetryMaxAttempts != 5 {
		t.Fatalf("Default max_retries was not set")
	}
	if b.bucketName != "tf-test" {
		t.Fatalf("Incorrect bucketName was populated")
	}
	if b.keyName != "state" {
		t.Fatalf("Incorrect keyName was populated")
	}

	// checkClientEndpoint(t, b.s3Client.Config, "")

	// checkClientEndpoint(t, b.dynClient.Config, "")

	credentials, err := b.awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		t.Fatalf("Error when requesting credentials")
	}
	if credentials.AccessKeyID == "" {
		t.Fatalf("No Access Key Id was populated")
	}
	if credentials.SecretAccessKey == "" {
		t.Fatalf("No Secret Access Key was populated")
	}
}

// func checkClientEndpoint(t *testing.T, config aws.Config, expected string) {
// 	if a := aws.StringValue(config.Endpoint); a != expected {
// 		t.Errorf("expected endpoint %q, got %q", expected, a)
// 	}
// }

func TestBackendConfig_InvalidRegion(t *testing.T) {
	testACC(t)

	cases := map[string]struct {
		config        map[string]any
		expectedDiags tfdiags.Diagnostics
	}{
		"with region validation": {
			config: map[string]interface{}{
				"region":                      "nonesuch",
				"bucket":                      "tf-test",
				"key":                         "state",
				"skip_credentials_validation": true,
			},
			expectedDiags: tfdiags.Diagnostics{
				tfdiags.AttributeValue(
					tfdiags.Error,
					"Invalid region value",
					`Invalid AWS Region: nonesuch`,
					cty.GetAttrPath("region"),
				),
			},
		},
		"skip region validation": {
			config: map[string]interface{}{
				"region":                      "nonesuch",
				"bucket":                      "tf-test",
				"key":                         "state",
				"skip_region_validation":      true,
				"skip_credentials_validation": true,
			},
			expectedDiags: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := New()
			configSchema := populateSchema(t, b.ConfigSchema(), hcl2shim.HCL2ValueFromConfigValue(tc.config))

			configSchema, diags := b.PrepareConfig(configSchema)
			if len(diags) > 0 {
				t.Fatal(diags.ErrWithWarnings())
			}

			confDiags := b.Configure(configSchema)
			diags = diags.Append(confDiags)

			if diff := cmp.Diff(diags, tc.expectedDiags, cmp.Comparer(diagnosticComparer)); diff != "" {
				t.Errorf("unexpected diagnostics difference: %s", diff)
			}
		})
	}
}

func TestBackendConfig_RegionEnvVar(t *testing.T) {
	testACC(t)
	config := map[string]interface{}{
		"bucket": "tf-test",
		"key":    "state",
	}

	cases := map[string]struct {
		vars map[string]string
	}{
		"AWS_REGION": {
			vars: map[string]string{
				"AWS_REGION": "us-west-1",
			},
		},

		"AWS_DEFAULT_REGION": {
			vars: map[string]string{
				"AWS_DEFAULT_REGION": "us-west-1",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.vars {
				os.Setenv(k, v)
			}
			t.Cleanup(func() {
				for k := range tc.vars {
					os.Unsetenv(k)
				}
			})

			b := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(config)).(*Backend)

			if b.awsConfig.Region != "us-west-1" {
				t.Fatalf("Incorrect region was populated")
			}
		})
	}
}

// func TestBackendConfig_DynamoDBEndpoint(t *testing.T) {
// 	testACC(t)

// 	cases := map[string]struct {
// 		config   map[string]any
// 		vars     map[string]string
// 		expected string
// 	}{
// 		"none": {
// 			expected: "",
// 		},
// 		"config": {
// 			config: map[string]any{
// 				"dynamodb_endpoint": "dynamo.test",
// 			},
// 			expected: "dynamo.test",
// 		},
// 		"envvar": {
// 			vars: map[string]string{
// 				"AWS_DYNAMODB_ENDPOINT": "dynamo.test",
// 			},
// 			expected: "dynamo.test",
// 		},
// 	}

// 	for name, tc := range cases {
// 		t.Run(name, func(t *testing.T) {
// 			config := map[string]interface{}{
// 				"region": "us-west-1",
// 				"bucket": "tf-test",
// 				"key":    "state",
// 			}

// 			if tc.vars != nil {
// 				for k, v := range tc.vars {
// 					os.Setenv(k, v)
// 				}
// 				t.Cleanup(func() {
// 					for k := range tc.vars {
// 						os.Unsetenv(k)
// 					}
// 				})
// 			}

// 			if tc.config != nil {
// 				for k, v := range tc.config {
// 					config[k] = v
// 				}
// 			}

// 			b := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(config)).(*Backend)

// 			checkClientEndpoint(t, b.dynClient.Config, tc.expected)
// 		})
// 	}
// }

// func TestBackendConfig_S3Endpoint(t *testing.T) {
// 	testACC(t)

// 	cases := map[string]struct {
// 		config   map[string]any
// 		vars     map[string]string
// 		expected string
// 	}{
// 		"none": {
// 			expected: "",
// 		},
// 		"config": {
// 			config: map[string]any{
// 				"endpoint": "s3.test",
// 			},
// 			expected: "s3.test",
// 		},
// 		"envvar": {
// 			vars: map[string]string{
// 				"AWS_S3_ENDPOINT": "s3.test",
// 			},
// 			expected: "s3.test",
// 		},
// 	}

// 	for name, tc := range cases {
// 		t.Run(name, func(t *testing.T) {
// 			config := map[string]interface{}{
// 				"region": "us-west-1",
// 				"bucket": "tf-test",
// 				"key":    "state",
// 			}

// 			if tc.vars != nil {
// 				for k, v := range tc.vars {
// 					os.Setenv(k, v)
// 				}
// 				t.Cleanup(func() {
// 					for k := range tc.vars {
// 						os.Unsetenv(k)
// 					}
// 				})
// 			}

// 			if tc.config != nil {
// 				for k, v := range tc.config {
// 					config[k] = v
// 				}
// 			}

// 			b := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(config)).(*Backend)

// 			checkClientEndpoint(t, b.s3Client.Config, tc.expected)
// 		})
// 	}
// }

func TestBackendConfig_AssumeRole(t *testing.T) {
	testACC(t)

	testCases := []struct {
		Config           map[string]interface{}
		Description      string
		MockStsEndpoints []*servicemocks.MockEndpoint
	}{
		{
			Config: map[string]interface{}{
				"bucket":       "tf-test",
				"key":          "state",
				"region":       "us-west-1",
				"role_arn":     servicemocks.MockStsAssumeRoleArn,
				"session_name": servicemocks.MockStsAssumeRoleSessionName,
			},
			Description: "role_arn",
			MockStsEndpoints: []*servicemocks.MockEndpoint{
				{
					Request: &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: url.Values{
						"Action":          []string{"AssumeRole"},
						"DurationSeconds": []string{"900"},
						"RoleArn":         []string{servicemocks.MockStsAssumeRoleArn},
						"RoleSessionName": []string{servicemocks.MockStsAssumeRoleSessionName},
						"Version":         []string{"2011-06-15"},
					}.Encode()},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsAssumeRoleValidResponseBody, ContentType: "text/xml"},
				},
				{
					Request:  &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: mockStsGetCallerIdentityRequestBody},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsGetCallerIdentityValidResponseBody, ContentType: "text/xml"},
				},
			},
		},
		{
			Config: map[string]interface{}{
				"assume_role_duration_seconds": 3600,
				"bucket":                       "tf-test",
				"key":                          "state",
				"region":                       "us-west-1",
				"role_arn":                     servicemocks.MockStsAssumeRoleArn,
				"session_name":                 servicemocks.MockStsAssumeRoleSessionName,
			},
			Description: "assume_role_duration_seconds",
			MockStsEndpoints: []*servicemocks.MockEndpoint{
				{
					Request: &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: url.Values{
						"Action":          []string{"AssumeRole"},
						"DurationSeconds": []string{"3600"},
						"RoleArn":         []string{servicemocks.MockStsAssumeRoleArn},
						"RoleSessionName": []string{servicemocks.MockStsAssumeRoleSessionName},
						"Version":         []string{"2011-06-15"},
					}.Encode()},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsAssumeRoleValidResponseBody, ContentType: "text/xml"},
				},
				{
					Request:  &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: mockStsGetCallerIdentityRequestBody},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsGetCallerIdentityValidResponseBody, ContentType: "text/xml"},
				},
			},
		},
		{
			Config: map[string]interface{}{
				"bucket":       "tf-test",
				"external_id":  servicemocks.MockStsAssumeRoleExternalId,
				"key":          "state",
				"region":       "us-west-1",
				"role_arn":     servicemocks.MockStsAssumeRoleArn,
				"session_name": servicemocks.MockStsAssumeRoleSessionName,
			},
			Description: "external_id",
			MockStsEndpoints: []*servicemocks.MockEndpoint{
				{
					Request: &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: url.Values{
						"Action":          []string{"AssumeRole"},
						"DurationSeconds": []string{"900"},
						"ExternalId":      []string{servicemocks.MockStsAssumeRoleExternalId},
						"RoleArn":         []string{servicemocks.MockStsAssumeRoleArn},
						"RoleSessionName": []string{servicemocks.MockStsAssumeRoleSessionName},
						"Version":         []string{"2011-06-15"},
					}.Encode()},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsAssumeRoleValidResponseBody, ContentType: "text/xml"},
				},
				{
					Request:  &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: mockStsGetCallerIdentityRequestBody},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsGetCallerIdentityValidResponseBody, ContentType: "text/xml"},
				},
			},
		},
		{
			Config: map[string]interface{}{
				"assume_role_policy": servicemocks.MockStsAssumeRolePolicy,
				"bucket":             "tf-test",
				"key":                "state",
				"region":             "us-west-1",
				"role_arn":           servicemocks.MockStsAssumeRoleArn,
				"session_name":       servicemocks.MockStsAssumeRoleSessionName,
			},
			Description: "assume_role_policy",
			MockStsEndpoints: []*servicemocks.MockEndpoint{
				{
					Request: &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: url.Values{
						"Action":          []string{"AssumeRole"},
						"DurationSeconds": []string{"900"},
						"Policy":          []string{servicemocks.MockStsAssumeRolePolicy},
						"RoleArn":         []string{servicemocks.MockStsAssumeRoleArn},
						"RoleSessionName": []string{servicemocks.MockStsAssumeRoleSessionName},
						"Version":         []string{"2011-06-15"},
					}.Encode()},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsAssumeRoleValidResponseBody, ContentType: "text/xml"},
				},
				{
					Request:  &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: mockStsGetCallerIdentityRequestBody},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsGetCallerIdentityValidResponseBody, ContentType: "text/xml"},
				},
			},
		},
		{
			Config: map[string]interface{}{
				"assume_role_policy_arns": []interface{}{servicemocks.MockStsAssumeRolePolicyArn},
				"bucket":                  "tf-test",
				"key":                     "state",
				"region":                  "us-west-1",
				"role_arn":                servicemocks.MockStsAssumeRoleArn,
				"session_name":            servicemocks.MockStsAssumeRoleSessionName,
			},
			Description: "assume_role_policy_arns",
			MockStsEndpoints: []*servicemocks.MockEndpoint{
				{
					Request: &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: url.Values{
						"Action":                  []string{"AssumeRole"},
						"DurationSeconds":         []string{"900"},
						"PolicyArns.member.1.arn": []string{servicemocks.MockStsAssumeRolePolicyArn},
						"RoleArn":                 []string{servicemocks.MockStsAssumeRoleArn},
						"RoleSessionName":         []string{servicemocks.MockStsAssumeRoleSessionName},
						"Version":                 []string{"2011-06-15"},
					}.Encode()},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsAssumeRoleValidResponseBody, ContentType: "text/xml"},
				},
				{
					Request:  &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: mockStsGetCallerIdentityRequestBody},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsGetCallerIdentityValidResponseBody, ContentType: "text/xml"},
				},
			},
		},
		{
			Config: map[string]interface{}{
				"assume_role_tags": map[string]interface{}{
					servicemocks.MockStsAssumeRoleTagKey: servicemocks.MockStsAssumeRoleTagValue,
				},
				"bucket":       "tf-test",
				"key":          "state",
				"region":       "us-west-1",
				"role_arn":     servicemocks.MockStsAssumeRoleArn,
				"session_name": servicemocks.MockStsAssumeRoleSessionName,
			},
			Description: "assume_role_tags",
			MockStsEndpoints: []*servicemocks.MockEndpoint{
				{
					Request: &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: url.Values{
						"Action":              []string{"AssumeRole"},
						"DurationSeconds":     []string{"900"},
						"RoleArn":             []string{servicemocks.MockStsAssumeRoleArn},
						"RoleSessionName":     []string{servicemocks.MockStsAssumeRoleSessionName},
						"Tags.member.1.Key":   []string{servicemocks.MockStsAssumeRoleTagKey},
						"Tags.member.1.Value": []string{servicemocks.MockStsAssumeRoleTagValue},
						"Version":             []string{"2011-06-15"},
					}.Encode()},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsAssumeRoleValidResponseBody, ContentType: "text/xml"},
				},
				{
					Request:  &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: mockStsGetCallerIdentityRequestBody},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsGetCallerIdentityValidResponseBody, ContentType: "text/xml"},
				},
			},
		},
		{
			Config: map[string]interface{}{
				"assume_role_tags": map[string]interface{}{
					servicemocks.MockStsAssumeRoleTagKey: servicemocks.MockStsAssumeRoleTagValue,
				},
				"assume_role_transitive_tag_keys": []interface{}{servicemocks.MockStsAssumeRoleTagKey},
				"bucket":                          "tf-test",
				"key":                             "state",
				"region":                          "us-west-1",
				"role_arn":                        servicemocks.MockStsAssumeRoleArn,
				"session_name":                    servicemocks.MockStsAssumeRoleSessionName,
			},
			Description: "assume_role_transitive_tag_keys",
			MockStsEndpoints: []*servicemocks.MockEndpoint{
				{
					Request: &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: url.Values{
						"Action":                     []string{"AssumeRole"},
						"DurationSeconds":            []string{"900"},
						"RoleArn":                    []string{servicemocks.MockStsAssumeRoleArn},
						"RoleSessionName":            []string{servicemocks.MockStsAssumeRoleSessionName},
						"Tags.member.1.Key":          []string{servicemocks.MockStsAssumeRoleTagKey},
						"Tags.member.1.Value":        []string{servicemocks.MockStsAssumeRoleTagValue},
						"TransitiveTagKeys.member.1": []string{servicemocks.MockStsAssumeRoleTagKey},
						"Version":                    []string{"2011-06-15"},
					}.Encode()},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsAssumeRoleValidResponseBody, ContentType: "text/xml"},
				},
				{
					Request:  &servicemocks.MockRequest{Method: "POST", Uri: "/", Body: mockStsGetCallerIdentityRequestBody},
					Response: &servicemocks.MockResponse{StatusCode: 200, Body: servicemocks.MockStsGetCallerIdentityValidResponseBody, ContentType: "text/xml"},
				},
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.Description, func(t *testing.T) {
			closeSts, _, stsEndpoint := mockdata.GetMockedAwsApiSession("STS", testCase.MockStsEndpoints)
			defer closeSts()

			testCase.Config["sts_endpoint"] = stsEndpoint

			b := New()
			diags := b.Configure(populateSchema(t, b.ConfigSchema(), hcl2shim.HCL2ValueFromConfigValue(testCase.Config)))

			if diags.HasErrors() {
				for _, diag := range diags {
					t.Errorf("unexpected error: %s", diag.Description().Summary)
				}
			}
		})
	}
}

func TestBackendConfig_PrepareConfigValidation(t *testing.T) {
	cases := map[string]struct {
		config        cty.Value
		expectedDiags tfdiags.Diagnostics
	}{
		"null bucket": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.NullVal(cty.String),
				"key":    cty.StringVal("test"),
				"region": cty.StringVal("us-west-2"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				requiredAttributeErrDiag(cty.GetAttrPath("bucket")),
			},
		},
		"empty bucket": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal(""),
				"key":    cty.StringVal("test"),
				"region": cty.StringVal("us-west-2"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value",
					"The value cannot be empty or all whitespace",
					cty.GetAttrPath("bucket"),
				),
			},
		},

		"null key": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.NullVal(cty.String),
				"region": cty.StringVal("us-west-2"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				requiredAttributeErrDiag(cty.GetAttrPath("key")),
			},
		},
		"empty key": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.StringVal(""),
				"region": cty.StringVal("us-west-2"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value",
					"The value cannot be empty or all whitespace",
					cty.GetAttrPath("key"),
				),
			},
		},
		"key with leading slash": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.StringVal("/leading-slash"),
				"region": cty.StringVal("us-west-2"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value",
					`The value must not start or end with "/"`,
					cty.GetAttrPath("key"),
				),
			},
		},
		"key with trailing slash": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.StringVal("trailing-slash/"),
				"region": cty.StringVal("us-west-2"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value",
					`The value must not start or end with "/"`,
					cty.GetAttrPath("key"),
				),
			},
		},

		"null region": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.StringVal("test"),
				"region": cty.NullVal(cty.String),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Missing region value",
					`The "region" attribute or the "AWS_REGION" or "AWS_DEFAULT_REGION" environment variables must be set.`,
					cty.GetAttrPath("region"),
				),
			},
		},
		"empty region": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.StringVal("test"),
				"region": cty.StringVal(""),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Missing region value",
					`The "region" attribute or the "AWS_REGION" or "AWS_DEFAULT_REGION" environment variables must be set.`,
					cty.GetAttrPath("region"),
				),
			},
		},

		"workspace_key_prefix with leading slash": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket":               cty.StringVal("test"),
				"key":                  cty.StringVal("test"),
				"region":               cty.StringVal("us-west-2"),
				"workspace_key_prefix": cty.StringVal("/env"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value",
					`The value must not start or end with "/"`,
					cty.GetAttrPath("workspace_key_prefix"),
				),
			},
		},
		"workspace_key_prefix with trailing slash": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket":               cty.StringVal("test"),
				"key":                  cty.StringVal("test"),
				"region":               cty.StringVal("us-west-2"),
				"workspace_key_prefix": cty.StringVal("env/"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value",
					`The value must not start or end with "/"`,
					cty.GetAttrPath("workspace_key_prefix"),
				),
			},
		},

		"encyrption key conflict": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket":               cty.StringVal("test"),
				"key":                  cty.StringVal("test"),
				"region":               cty.StringVal("us-west-2"),
				"workspace_key_prefix": cty.StringVal("env"),
				"sse_customer_key":     cty.StringVal("1hwbcNPGWL+AwDiyGmRidTWAEVmCWMKbEHA+Es8w75o="),
				"kms_key_id":           cty.StringVal("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-ab56-1234567890ab"),
			}),
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Attribute Combination",
					`Only one of kms_key_id, sse_customer_key can be set.`,
					cty.Path{},
				),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			oldEnv := servicemocks.StashEnv()
			defer servicemocks.PopEnv(oldEnv)

			b := New()

			_, valDiags := b.PrepareConfig(populateSchema(t, b.ConfigSchema(), tc.config))

			if diff := cmp.Diff(valDiags, tc.expectedDiags, cmp.Comparer(diagnosticComparer)); diff != "" {
				t.Errorf("unexpected diagnostics difference: %s", diff)
			}
		})
	}
}

func TestBackendConfig_PrepareConfigWithEnvVars(t *testing.T) {
	cases := map[string]struct {
		config      cty.Value
		vars        map[string]string
		expectedErr string
	}{
		"region env var AWS_REGION": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.StringVal("test"),
				"region": cty.NullVal(cty.String),
			}),
			vars: map[string]string{
				"AWS_REGION": "us-west-1",
			},
		},
		"region env var AWS_DEFAULT_REGION": {
			config: cty.ObjectVal(map[string]cty.Value{
				"bucket": cty.StringVal("test"),
				"key":    cty.StringVal("test"),
				"region": cty.NullVal(cty.String),
			}),
			vars: map[string]string{
				"AWS_DEFAULT_REGION": "us-west-1",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			oldEnv := servicemocks.StashEnv()
			defer servicemocks.PopEnv(oldEnv)

			b := New()

			for k, v := range tc.vars {
				os.Setenv(k, v)
			}

			_, valDiags := b.PrepareConfig(populateSchema(t, b.ConfigSchema(), tc.config))
			if tc.expectedErr != "" {
				if valDiags.Err() != nil {
					actualErr := valDiags.Err().Error()
					if !strings.Contains(actualErr, tc.expectedErr) {
						t.Fatalf("unexpected validation result: %v", valDiags.Err())
					}
				} else {
					t.Fatal("expected an error, got none")
				}
			} else if valDiags.Err() != nil {
				t.Fatalf("expected no error, got %s", valDiags.Err())
			}
		})
	}
}

func TestBackend(t *testing.T) {
	testACC(t)

	ctx := context.TODO()

	bucketName := fmt.Sprintf("terraform-remote-s3-test-%x", time.Now().Unix())
	keyName := "testState"

	b := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":  bucketName,
		"key":     keyName,
		"encrypt": true,
		"region":  "us-west-1",
	})).(*Backend)

	createS3Bucket(ctx, t, b.s3Client, bucketName, b.awsConfig.Region)
	defer deleteS3Bucket(ctx, t, b.s3Client, bucketName)

	backend.TestBackendStates(t, b)
}

func TestBackendLocked(t *testing.T) {
	testACC(t)

	ctx := context.TODO()

	bucketName := fmt.Sprintf("terraform-remote-s3-test-%x", time.Now().Unix())
	keyName := "test/state"

	b1 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":         bucketName,
		"key":            keyName,
		"encrypt":        true,
		"dynamodb_table": bucketName,
		"region":         "us-west-1",
	})).(*Backend)

	b2 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":         bucketName,
		"key":            keyName,
		"encrypt":        true,
		"dynamodb_table": bucketName,
		"region":         "us-west-1",
	})).(*Backend)

	createS3Bucket(ctx, t, b1.s3Client, bucketName, b1.awsConfig.Region)
	defer deleteS3Bucket(ctx, t, b1.s3Client, bucketName)
	createDynamoDBTable(ctx, t, b1.dynClient, bucketName)
	defer deleteDynamoDBTable(ctx, t, b1.dynClient, bucketName)

	backend.TestBackendStateLocks(t, b1, b2)
	backend.TestBackendStateForceUnlock(t, b1, b2)
}

func TestBackendKmsKeyId(t *testing.T) {
	testACC(t)

	testCases := map[string]struct {
		config        map[string]any
		expectedKeyId string
		expectedDiags tfdiags.Diagnostics
	}{
		"valid": {
			config: map[string]any{
				"kms_key_id": "arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-ab56-1234567890ab",
			},
			expectedKeyId: "arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-ab56-1234567890ab",
		},

		"invalid": {
			config: map[string]any{
				"kms_key_id": "not-an-arn",
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid KMS Key ID",
					`Value must be a valid KMS Key ID, got "not-an-arn"`,
					cty.GetAttrPath("kms_key_id"),
				),
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			bucketName := fmt.Sprintf("terraform-remote-s3-test-%x", time.Now().Unix())
			config := map[string]any{
				"bucket":  bucketName,
				"encrypt": true,
				"key":     "test-SSE-KMS",
				"region":  "us-west-1",
			}
			maps.Copy(config, tc.config)

			b := New().(*Backend)
			configSchema := populateSchema(t, b.ConfigSchema(), hcl2shim.HCL2ValueFromConfigValue(config))

			configSchema, diags := b.PrepareConfig(configSchema)

			if !diags.HasErrors() {
				confDiags := b.Configure(configSchema)
				diags = diags.Append(confDiags)
			}

			if diff := cmp.Diff(diags, tc.expectedDiags, cmp.Comparer(diagnosticComparer)); diff != "" {
				t.Fatalf("unexpected diagnostics difference: %s", diff)
			}

			if tc.expectedKeyId != "" {
				if string(b.kmsKeyID) != tc.expectedKeyId {
					t.Fatal("unexpected value for KMS key Id")
				}
			}
		})
	}
}

func TestBackendSSECustomerKey(t *testing.T) {
	testACC(t)

	ctx := context.TODO()

	testCases := map[string]struct {
		config               map[string]any
		environmentVariables map[string]string
		expectedKey          string
		expectedDiags        tfdiags.Diagnostics
	}{
		// config
		"config valid": {
			config: map[string]any{
				"sse_customer_key": "4Dm1n4rphuFgawxuzY/bEfvLf6rYK0gIjfaDSLlfXNk=",
			},
			expectedKey: string(must(base64.StdEncoding.DecodeString("4Dm1n4rphuFgawxuzY/bEfvLf6rYK0gIjfaDSLlfXNk="))),
		},
		"config invalid length": {
			config: map[string]any{
				"sse_customer_key": "test",
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid sse_customer_key value",
					"sse_customer_key must be 44 characters in length",
					cty.GetAttrPath("sse_customer_key"),
				),
			},
		},
		"config invalid encoding": {
			config: map[string]any{
				"sse_customer_key": "====CT70aTYB2JGff7AjQtwbiLkwH4npICay1PWtmdka",
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid sse_customer_key value",
					"sse_customer_key must be base64 encoded: illegal base64 data at input byte 0",
					cty.GetAttrPath("sse_customer_key"),
				),
			},
		},

		// env var
		"envvar valid": {
			environmentVariables: map[string]string{
				"AWS_SSE_CUSTOMER_KEY": "4Dm1n4rphuFgawxuzY/bEfvLf6rYK0gIjfaDSLlfXNk=",
			},
			expectedKey: string(must(base64.StdEncoding.DecodeString("4Dm1n4rphuFgawxuzY/bEfvLf6rYK0gIjfaDSLlfXNk="))),
		},
		"envvar invalid length": {
			environmentVariables: map[string]string{
				"AWS_SSE_CUSTOMER_KEY": "test",
			},
			expectedDiags: tfdiags.Diagnostics{
				wholeBodyErrDiag(
					"Invalid AWS_SSE_CUSTOMER_KEY value",
					`The environment variable "AWS_SSE_CUSTOMER_KEY" must be 44 characters in length`,
				),
			},
		},
		"envvar invalid encoding": {
			environmentVariables: map[string]string{
				"AWS_SSE_CUSTOMER_KEY": "====CT70aTYB2JGff7AjQtwbiLkwH4npICay1PWtmdka",
			},
			expectedDiags: tfdiags.Diagnostics{
				wholeBodyErrDiag(
					"Invalid AWS_SSE_CUSTOMER_KEY value",
					`The environment variable "AWS_SSE_CUSTOMER_KEY" must be base64 encoded: illegal base64 data at input byte 0`,
				),
			},
		},

		// conflict
		"config kms_key_id and envvar AWS_SSE_CUSTOMER_KEY": {
			config: map[string]any{
				"kms_key_id": "arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-ab56-1234567890ab",
			},
			environmentVariables: map[string]string{
				"AWS_SSE_CUSTOMER_KEY": "4Dm1n4rphuFgawxuzY/bEfvLf6rYK0gIjfaDSLlfXNk=",
			},
			expectedDiags: tfdiags.Diagnostics{
				wholeBodyErrDiag(
					"Invalid encryption configuration",
					encryptionKeyConflictEnvVarError,
				),
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			bucketName := fmt.Sprintf("terraform-remote-s3-test-%x", time.Now().Unix())
			config := map[string]any{
				"bucket":  bucketName,
				"encrypt": true,
				"key":     "test-SSE-C",
				"region":  "us-west-1",
			}
			maps.Copy(config, tc.config)

			oldEnv := os.Environ() // For now, save without clearing
			defer servicemocks.PopEnv(oldEnv)
			for k, v := range tc.environmentVariables {
				os.Setenv(k, v)
			}

			b := New().(*Backend)
			configSchema := populateSchema(t, b.ConfigSchema(), hcl2shim.HCL2ValueFromConfigValue(config))

			configSchema, diags := b.PrepareConfig(configSchema)

			if !diags.HasErrors() {
				confDiags := b.Configure(configSchema)
				diags = diags.Append(confDiags)
			}

			if diff := cmp.Diff(diags, tc.expectedDiags, cmp.Comparer(diagnosticComparer)); diff != "" {
				t.Fatalf("unexpected diagnostics difference: %s", diff)
			}

			if tc.expectedKey != "" {
				if string(b.customerEncryptionKey) != tc.expectedKey {
					t.Fatal("unexpected value for customer encryption key")
				}
			}

			if !diags.HasErrors() {
				createS3Bucket(ctx, t, b.s3Client, bucketName, b.awsConfig.Region)
				defer deleteS3Bucket(ctx, t, b.s3Client, bucketName)

				backend.TestBackendStates(t, b)
			}
		})
	}
}

// add some extra junk in S3 to try and confuse the env listing.
func TestBackendExtraPaths(t *testing.T) {
	testACC(t)

	ctx := context.TODO()

	bucketName := fmt.Sprintf("terraform-remote-s3-test-%x", time.Now().Unix())
	keyName := "test/state/tfstate"

	b := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":  bucketName,
		"key":     keyName,
		"encrypt": true,
	})).(*Backend)

	createS3Bucket(ctx, t, b.s3Client, bucketName, b.awsConfig.Region)
	defer deleteS3Bucket(ctx, t, b.s3Client, bucketName)

	// put multiple states in old env paths.
	s1 := states.NewState()
	s2 := states.NewState()

	// RemoteClient to Put things in various paths
	client := &RemoteClient{
		s3Client:             b.s3Client,
		dynClient:            b.dynClient,
		bucketName:           b.bucketName,
		path:                 b.path("s1"),
		serverSideEncryption: b.serverSideEncryption,
		acl:                  b.acl,
		kmsKeyID:             b.kmsKeyID,
		ddbTable:             b.ddbTable,
	}

	// Write the first state
	stateMgr := &remote.State{Client: client}
	if err := stateMgr.WriteState(s1); err != nil {
		t.Fatal(err)
	}
	if err := stateMgr.PersistState(nil); err != nil {
		t.Fatal(err)
	}

	// Write the second state
	// Note a new state manager - otherwise, because these
	// states are equal, the state will not Put to the remote
	client.path = b.path("s2")
	stateMgr2 := &remote.State{Client: client}
	if err := stateMgr2.WriteState(s2); err != nil {
		t.Fatal(err)
	}
	if err := stateMgr2.PersistState(nil); err != nil {
		t.Fatal(err)
	}

	s2Lineage := stateMgr2.StateSnapshotMeta().Lineage

	if err := checkStateList(b, []string{"default", "s1", "s2"}); err != nil {
		t.Fatal(err)
	}

	// put a state in an env directory name
	client.path = b.workspaceKeyPrefix + "/error"
	if err := stateMgr.WriteState(states.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := stateMgr.PersistState(nil); err != nil {
		t.Fatal(err)
	}
	if err := checkStateList(b, []string{"default", "s1", "s2"}); err != nil {
		t.Fatal(err)
	}

	// add state with the wrong key for an existing env
	client.path = b.workspaceKeyPrefix + "/s2/notTestState"
	if err := stateMgr.WriteState(states.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := stateMgr.PersistState(nil); err != nil {
		t.Fatal(err)
	}
	if err := checkStateList(b, []string{"default", "s1", "s2"}); err != nil {
		t.Fatal(err)
	}

	// remove the state with extra subkey
	if err := client.Delete(); err != nil {
		t.Fatal(err)
	}

	// delete the real workspace
	if err := b.DeleteWorkspace("s2", true); err != nil {
		t.Fatal(err)
	}

	if err := checkStateList(b, []string{"default", "s1"}); err != nil {
		t.Fatal(err)
	}

	// fetch that state again, which should produce a new lineage
	s2Mgr, err := b.StateMgr("s2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s2Mgr.RefreshState(); err != nil {
		t.Fatal(err)
	}

	if s2Mgr.(*remote.State).StateSnapshotMeta().Lineage == s2Lineage {
		t.Fatal("state s2 was not deleted")
	}
	_ = s2Mgr.State() // We need the side-effect
	s2Lineage = stateMgr.StateSnapshotMeta().Lineage

	// add a state with a key that matches an existing environment dir name
	client.path = b.workspaceKeyPrefix + "/s2/"
	if err := stateMgr.WriteState(states.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := stateMgr.PersistState(nil); err != nil {
		t.Fatal(err)
	}

	// make sure s2 is OK
	s2Mgr, err = b.StateMgr("s2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s2Mgr.RefreshState(); err != nil {
		t.Fatal(err)
	}

	if stateMgr.StateSnapshotMeta().Lineage != s2Lineage {
		t.Fatal("we got the wrong state for s2")
	}

	if err := checkStateList(b, []string{"default", "s1", "s2"}); err != nil {
		t.Fatal(err)
	}
}

// ensure we can separate the workspace prefix when it also matches the prefix
// of the workspace name itself.
func TestBackendPrefixInWorkspace(t *testing.T) {
	testACC(t)

	ctx := context.TODO()

	bucketName := fmt.Sprintf("terraform-remote-s3-test-%x", time.Now().Unix())

	b := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":               bucketName,
		"key":                  "test-env.tfstate",
		"workspace_key_prefix": "env",
	})).(*Backend)

	createS3Bucket(ctx, t, b.s3Client, bucketName, b.awsConfig.Region)
	defer deleteS3Bucket(ctx, t, b.s3Client, bucketName)

	// get a state that contains the prefix as a substring
	sMgr, err := b.StateMgr("env-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sMgr.RefreshState(); err != nil {
		t.Fatal(err)
	}

	if err := checkStateList(b, []string{"default", "env-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestKeyEnv(t *testing.T) {
	testACC(t)

	ctx := context.TODO()

	keyName := "some/paths/tfstate"

	bucket0Name := fmt.Sprintf("terraform-remote-s3-test-%x-0", time.Now().Unix())
	b0 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":               bucket0Name,
		"key":                  keyName,
		"encrypt":              true,
		"workspace_key_prefix": "",
	})).(*Backend)

	createS3Bucket(ctx, t, b0.s3Client, bucket0Name, b0.awsConfig.Region)
	defer deleteS3Bucket(ctx, t, b0.s3Client, bucket0Name)

	bucket1Name := fmt.Sprintf("terraform-remote-s3-test-%x-1", time.Now().Unix())
	b1 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":               bucket1Name,
		"key":                  keyName,
		"encrypt":              true,
		"workspace_key_prefix": "project/env:",
	})).(*Backend)

	createS3Bucket(ctx, t, b1.s3Client, bucket1Name, b1.awsConfig.Region)
	defer deleteS3Bucket(ctx, t, b1.s3Client, bucket1Name)

	bucket2Name := fmt.Sprintf("terraform-remote-s3-test-%x-2", time.Now().Unix())
	b2 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"bucket":  bucket2Name,
		"key":     keyName,
		"encrypt": true,
	})).(*Backend)

	createS3Bucket(ctx, t, b2.s3Client, bucket2Name, b2.awsConfig.Region)
	defer deleteS3Bucket(ctx, t, b2.s3Client, bucket2Name)

	if err := testGetWorkspaceForKey(b0, "some/paths/tfstate", ""); err != nil {
		t.Fatal(err)
	}

	if err := testGetWorkspaceForKey(b0, "ws1/some/paths/tfstate", "ws1"); err != nil {
		t.Fatal(err)
	}

	if err := testGetWorkspaceForKey(b1, "project/env:/ws1/some/paths/tfstate", "ws1"); err != nil {
		t.Fatal(err)
	}

	if err := testGetWorkspaceForKey(b1, "project/env:/ws2/some/paths/tfstate", "ws2"); err != nil {
		t.Fatal(err)
	}

	if err := testGetWorkspaceForKey(b2, "env:/ws3/some/paths/tfstate", "ws3"); err != nil {
		t.Fatal(err)
	}

	backend.TestBackendStates(t, b0)
	backend.TestBackendStates(t, b1)
	backend.TestBackendStates(t, b2)
}

func TestAssumeRole_PrepareConfigValidation(t *testing.T) {
	path := cty.GetAttrPath("field")

	cases := map[string]struct {
		config        map[string]cty.Value
		expectedDiags tfdiags.Diagnostics
	}{
		"basic": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
			},
		},

		"invalid ARN": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("not an arn"),
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid ARN",
					`The value "not an arn" cannot be parsed as an ARN: arn: invalid prefix`,
					path.GetAttr("role_arn"),
				),
			},
		},

		"no role_arn": {
			config: map[string]cty.Value{},
			expectedDiags: tfdiags.Diagnostics{
				requiredAttributeErrDiag(path.GetAttr("role_arn")),
			},
		},

		"with duration": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"duration": cty.StringVal("2h"),
			},
		},

		"invalid duration": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"duration": cty.StringVal("two hours"),
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Duration",
					`The value "two hours" cannot be parsed as a duration: time: invalid duration "two hours"`,
					path.GetAttr("duration"),
				),
			},
		},

		"with external_id": {
			config: map[string]cty.Value{
				"role_arn":    cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"external_id": cty.StringVal("external-id"),
			},
		},

		"empty external_id": {
			config: map[string]cty.Value{
				"role_arn":    cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"external_id": cty.StringVal(""),
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value Length",
					`Length must be between 2 and 1224, had 0`,
					path.GetAttr("external_id"),
				),
			},
		},

		"with policy": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"policy":   cty.StringVal("{}"),
			},
		},

		"invalid policy": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"policy":   cty.StringVal(""),
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid Value",
					`The value cannot be empty or all whitespace`,
					path.GetAttr("policy"),
				),
			},
		},

		"with policy_arns": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"policy_arns": cty.SetVal([]cty.Value{
					cty.StringVal("arn:aws:iam::123456789012:policy/testpolicy"),
				}),
			},
		},

		"invalid policy_arns": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"policy_arns": cty.SetVal([]cty.Value{
					cty.StringVal("not an arn"),
				}),
			},
			expectedDiags: tfdiags.Diagnostics{
				attributeErrDiag(
					"Invalid ARN",
					`The value "not an arn" cannot be parsed as an ARN: arn: invalid prefix`,
					path.GetAttr("policy_arns").IndexString("not an arn"),
				),
			},
		},

		"with session_name": {
			config: map[string]cty.Value{
				"role_arn":     cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"session_name": cty.StringVal("session-name"),
			},
		},

		// NOT SUPPORTED by `aws-sdk-go-base/v1`
		// "source_identity"

		"with tags": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"tags": cty.MapVal(map[string]cty.Value{
					"tag-key": cty.StringVal("tag-value"),
				}),
			},
		},

		"with transitive_tag_keys": {
			config: map[string]cty.Value{
				"role_arn": cty.StringVal("arn:aws:iam::123456789012:role/testrole"),
				"transitive_tag_keys": cty.SetVal([]cty.Value{
					cty.StringVal("tag-key"),
				}),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			schema := assumeRoleFullSchema()
			vals := make(map[string]cty.Value, len(schema))
			for name, attrSchema := range schema {
				if val, ok := tc.config[name]; ok {
					vals[name] = val
				} else {
					vals[name] = cty.NullVal(attrSchema.SchemaAttribute().Type)
				}
			}
			config := cty.ObjectVal(vals)

			diags := prepareAssumeRoleConfig(config, path)

			if diff := cmp.Diff(diags, tc.expectedDiags, cmp.Comparer(diagnosticComparer)); diff != "" {
				t.Errorf("unexpected diagnostics difference: %s", diff)
			}
		})
	}
}

func testGetWorkspaceForKey(b *Backend, key string, expected string) error {
	if actual := b.keyEnv(key); actual != expected {
		return fmt.Errorf("incorrect workspace for key[%q]. Expected[%q]: Actual[%q]", key, expected, actual)
	}
	return nil
}

func checkStateList(b backend.Backend, expected []string) error {
	states, err := b.Workspaces()
	if err != nil {
		return err
	}

	if !reflect.DeepEqual(states, expected) {
		return fmt.Errorf("incorrect states listed: %q", states)
	}
	return nil
}

func createS3Bucket(ctx context.Context, t *testing.T, s3Client *s3.Client, bucketName, region string) {
	createBucketReq := &s3.CreateBucketInput{
		Bucket: &bucketName,
	}
	if region != "us-east-1" {
		createBucketReq.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}

	// Be clear about what we're doing in case the user needs to clean
	// this up later.
	t.Logf("creating S3 bucket %s in %s", bucketName, region)
	_, err := s3Client.CreateBucket(ctx, createBucketReq)
	if err != nil {
		t.Fatal("failed to create test S3 bucket:", err)
	}
}

func deleteS3Bucket(ctx context.Context, t *testing.T, s3Client *s3.Client, bucketName string) {
	warning := "WARNING: Failed to delete the test S3 bucket. It may have been left in your AWS account and may incur storage charges. (error was %s)"

	// first we have to get rid of the env objects, or we can't delete the bucket
	resp, err := s3Client.ListObjects(ctx, &s3.ListObjectsInput{Bucket: &bucketName})
	if err != nil {
		t.Logf(warning, err)
		return
	}
	for _, obj := range resp.Contents {
		if _, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucketName, Key: obj.Key}); err != nil {
			// this will need cleanup no matter what, so just warn and exit
			t.Logf(warning, err)
			return
		}
	}

	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucketName}); err != nil {
		t.Logf(warning, err)
	}
}

// create the dynamoDB table, and wait until we can query it.
func createDynamoDBTable(ctx context.Context, t *testing.T, dynClient *dynamodb.Client, tableName string) {
	createInput := &dynamodb.CreateTableInput{
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{
				AttributeName: aws.String("LockID"),
				AttributeType: dynamodbtypes.ScalarAttributeTypeS,
			},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{
				AttributeName: aws.String("LockID"),
				KeyType:       dynamodbtypes.KeyTypeHash,
			},
		},
		ProvisionedThroughput: &dynamodbtypes.ProvisionedThroughput{
			ReadCapacityUnits:  aws.Int64(5),
			WriteCapacityUnits: aws.Int64(5),
		},
		TableName: aws.String(tableName),
	}

	_, err := dynClient.CreateTable(ctx, createInput)
	if err != nil {
		t.Fatal(err)
	}

	// now wait until it's ACTIVE
	start := time.Now()
	time.Sleep(time.Second)

	describeInput := &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	}

	for {
		resp, err := dynClient.DescribeTable(ctx, describeInput)
		if err != nil {
			t.Fatal(err)
		}

		if resp.Table.TableStatus == dynamodbtypes.TableStatusActive {
			return
		}

		if time.Since(start) > time.Minute {
			t.Fatalf("timed out creating DynamoDB table %s", tableName)
		}

		time.Sleep(3 * time.Second)
	}

}

func deleteDynamoDBTable(ctx context.Context, t *testing.T, dynClient *dynamodb.Client, tableName string) {
	params := &dynamodb.DeleteTableInput{
		TableName: aws.String(tableName),
	}
	_, err := dynClient.DeleteTable(ctx, params)
	if err != nil {
		t.Logf("WARNING: Failed to delete the test DynamoDB table %q. It has been left in your AWS account and may incur charges. (error was %s)", tableName, err)
	}
}

func populateSchema(t *testing.T, schema *configschema.Block, value cty.Value) cty.Value {
	ty := schema.ImpliedType()
	var path cty.Path
	val, err := unmarshal(value, ty, path)
	if err != nil {
		t.Fatalf("populating schema: %s", err)
	}
	return val
}

func unmarshal(value cty.Value, ty cty.Type, path cty.Path) (cty.Value, error) {
	switch {
	case ty.IsPrimitiveType():
		return value, nil
	// case ty.IsListType():
	// 	return unmarshalList(value, ty.ElementType(), path)
	case ty.IsSetType():
		return unmarshalSet(value, ty.ElementType(), path)
	case ty.IsMapType():
		return unmarshalMap(value, ty.ElementType(), path)
	// case ty.IsTupleType():
	// 	return unmarshalTuple(value, ty.TupleElementTypes(), path)
	case ty.IsObjectType():
		return unmarshalObject(value, ty.AttributeTypes(), path)
	default:
		return cty.NilVal, path.NewErrorf("unsupported type %s", ty.FriendlyName())
	}
}

func unmarshalSet(dec cty.Value, ety cty.Type, path cty.Path) (cty.Value, error) {
	if dec.IsNull() {
		return dec, nil
	}

	length := dec.LengthInt()

	if length == 0 {
		return cty.SetValEmpty(ety), nil
	}

	vals := make([]cty.Value, 0, length)
	dec.ForEachElement(func(key, val cty.Value) (stop bool) {
		vals = append(vals, val)
		return
	})

	return cty.SetVal(vals), nil
}

func unmarshalMap(dec cty.Value, ety cty.Type, path cty.Path) (cty.Value, error) {
	if dec.IsNull() {
		return dec, nil
	}

	length := dec.LengthInt()

	if length == 0 {
		return cty.MapValEmpty(ety), nil
	}

	vals := make(map[string]cty.Value, length)
	dec.ForEachElement(func(key, val cty.Value) (stop bool) {
		k := stringValue(key)
		vals[k] = val
		return
	})

	return cty.MapVal(vals), nil
}

func unmarshalObject(dec cty.Value, atys map[string]cty.Type, path cty.Path) (cty.Value, error) {
	if dec.IsNull() {
		return dec, nil
	}
	valueTy := dec.Type()

	vals := make(map[string]cty.Value, len(atys))
	path = append(path, nil)
	for key, aty := range atys {
		path[len(path)-1] = cty.IndexStep{
			Key: cty.StringVal(key),
		}

		if !valueTy.HasAttribute(key) {
			vals[key] = cty.NullVal(aty)
		} else {
			val, err := unmarshal(dec.GetAttr(key), aty, path)
			if err != nil {
				return cty.DynamicVal, err
			}
			vals[key] = val
		}
	}

	return cty.ObjectVal(vals), nil
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	} else {
		return v
	}
}
