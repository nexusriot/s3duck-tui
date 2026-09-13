package model

import (
	"fmt"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// rangeNotSatisfiable is the transport-level shape of the same refusal: a bare
// 416 with no error code, which is what a backend that does not name the error
// returns.
func rangeNotSatisfiable() error {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{
				Response: &http.Response{StatusCode: http.StatusRequestedRangeNotSatisfiable},
			},
			Err: fmt.Errorf("range not satisfiable"),
		},
	}
}

func TestIsInvalidRange(t *testing.T) {
	// A zero-byte object has no satisfiable range, so the preview's ranged GET
	// is refused. It has to read as "empty", not as an error, and the
	// S3-compatible backends do not agree on how they spell the refusal.
	for _, err := range []error{
		&smithy.GenericAPIError{Code: "InvalidRange", Message: "The requested range is not satisfiable"},
		&smithy.GenericAPIError{Code: "RequestedRangeNotSatisfiable"},
		&smithy.GenericAPIError{Code: "416"},
		fmt.Errorf("reading the head of x: %w", &smithy.GenericAPIError{Code: "InvalidRange"}),
		rangeNotSatisfiable(),
	} {
		if !isInvalidRange(err) {
			t.Errorf("isInvalidRange(%v) = false, want true", err)
		}
	}

	for _, err := range []error{
		nil,
		fmt.Errorf("connection reset"),
		&smithy.GenericAPIError{Code: "NoSuchKey"},
		&smithy.GenericAPIError{Code: "AccessDenied"},
	} {
		if isInvalidRange(err) {
			t.Errorf("isInvalidRange(%v) = true, want false", err)
		}
	}
}
