package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

func TestBucketDashboardTextSaysWhyASectionIsEmpty(t *testing.T) {
	info := model.BucketConfigInfo{
		Region:            "eu-west-1",
		Versioning:        model.ConfigItem{State: model.ConfigSet, Value: "Enabled"},
		Encryption:        model.ConfigItem{State: model.ConfigDenied},
		ObjectLock:        model.ConfigItem{State: model.ConfigUnset, Value: "off"},
		PublicAccessBlock: model.ConfigItem{State: model.ConfigUnsupported},
		Tags:              model.ConfigItem{State: model.ConfigSet, Value: "1 tag(s)", Detail: []string{"env=[prod]"}},
		Lifecycle:         model.ConfigItem{State: model.ConfigError, Err: errors.New("timeout")},
	}
	got := rendered(bucketDashboardText("logs", info))
	for _, want := range []string{
		"Region               eu-west-1",
		"Versioning           Enabled",
		"Default encryption   access denied",
		"Object lock          off",
		"Public access block  not supported by this endpoint",
		"    env=[prod]", // a tag value that looks like markup survives
		"Lifecycle            could not read: timeout",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dashboard lacks %q:\n%s", want, got)
		}
	}
	// Policy is last: it is the one section that can run long.
	if strings.Index(got, "Policy") < strings.Index(got, "Access logging") {
		t.Error("the policy should come after every one-line section")
	}
}

func TestValidateBucketTags(t *testing.T) {
	if err := validateBucketTags(map[string]string{"env": "prod"}); err != nil {
		t.Errorf("a plain tag was refused: %v", err)
	}
	if err := validateBucketTags(map[string]string{"aws:cloudformation:stack-name": "x"}); err == nil {
		t.Error("an aws: key must be refused before S3 refuses it")
	}
	many := map[string]string{}
	for i := 0; i <= maxBucketTags; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	if err := validateBucketTags(many); err == nil {
		t.Error("more than 50 tags must be refused")
	}
	// Buckets allow more tags than objects do.
	eleven := map[string]string{}
	for i := 0; i < 11; i++ {
		eleven[strings.Repeat("t", i+1)] = "v"
	}
	if err := validateBucketTags(eleven); err != nil {
		t.Errorf("11 bucket tags are legal: %v", err)
	}
}

func TestSystemTagsAreFound(t *testing.T) {
	got := systemTags(map[string]string{"env": "p", "aws:cloudformation:stack-id": "s", "AWS:x": "y"})
	if len(got) != 2 {
		t.Errorf("systemTags = %v, want both aws: keys whatever their case", got)
	}
}
