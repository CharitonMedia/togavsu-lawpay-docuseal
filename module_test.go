package lawpaydocuseal

import (
	"strings"
	"testing"
)

// The manifest carries the PRD identity and every setting the module reads.
func TestManifestIdentityAndSettings(t *testing.T) {
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	m := string(files[ManifestFile])
	for _, line := range []string{"id: lawpay-docuseal", `version: "1.0.0"`, "author: Chariton Media", "license: Same as Togavsu", "price: Free"} {
		if !strings.Contains(m, "\n"+line+"\n") {
			t.Errorf("manifest lacks %q", line)
		}
	}
	defaults := manifestDefaults(t)
	for _, k := range []string{"core_trust_account_id", "docuseal_api_key", "docuseal_base_url", "docuseal_template_id", "docuseal_webhook_token",
		"firm_signer_email", "firm_signer_name", "invoice_pay_destination", "lawpay_api_base", "lawpay_hosted_fields_url",
		"lawpay_operating_account_id", "lawpay_public_key", "lawpay_secret_key", "lawpay_trust_account_id", "lawpay_webhook_token",
		"max_charge_cents", "min_charge_cents", "pay_attempt_window_minutes", "pay_attempts_per_window", "pay_intent_minutes",
		"pending_check_days", "reconcile_interval_minutes", "retry_limit", "signing_expire_days"} {
		if _, ok := defaults[k]; !ok {
			t.Errorf("setting %s not in the manifest", k)
		}
	}
	for _, k := range secretSettings {
		if defaults[k] != "" {
			t.Errorf("secret %s has a default", k)
		}
	}
}
