package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelAttachmentsKeyParses(t *testing.T) {
	var off, unset ModelConfig
	if err := yaml.Unmarshal([]byte("id: a\nattachments: false\n"), &off); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte("id: a\n"), &unset); err != nil {
		t.Fatal(err)
	}
	if off.Attachments == nil || *off.Attachments {
		t.Errorf("attachments: false parsed to %v", off.Attachments)
	}
	if unset.Attachments != nil {
		t.Errorf("unset parsed to %v", *unset.Attachments)
	}
}
