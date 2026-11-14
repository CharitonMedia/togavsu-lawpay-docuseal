-- DocuSeal records for fee agreements. Contact details are read from the core at send time
-- and never stored here. The foreign keys to core.fee_agreements are in
-- 0004_fee_agreement_links.sql.

-- The declared extension of the core fee agreement (include=lawpay-docuseal.agreement_details).
CREATE TABLE agreement_details (
  fee_agreement_id TEXT PRIMARY KEY,
  case_number TEXT NOT NULL DEFAULT '' CHECK (length(case_number) <= 100),
  new_case BOOLEAN,
  updated_at TIMESTAMPTZ NOT NULL
);

-- One DocuSeal submission per send of a fee agreement.
CREATE TABLE signings (
  id TEXT PRIMARY KEY,
  fee_agreement_id TEXT NOT NULL,
  matter_id TEXT NOT NULL REFERENCES core.matters (id) ON DELETE RESTRICT,
  client_contact_id TEXT NOT NULL REFERENCES core.contacts (id) ON DELETE RESTRICT,
  state TEXT NOT NULL CHECK (state IN ('sending', 'sent', 'client_signed', 'processing_post_execution', 'executed',
    'declined', 'expired', 'voiding', 'voided', 'error')),
  prior_state TEXT,
  docuseal_submission_id TEXT UNIQUE,
  client_submitter_id TEXT NOT NULL DEFAULT '',
  firm_submitter_id TEXT NOT NULL DEFAULT '',
  client_signing_url TEXT NOT NULL DEFAULT '',
  audit_url TEXT NOT NULL DEFAULT '',
  sent_at TIMESTAMPTZ,
  client_signed_at TIMESTAMPTZ,
  client_signed_during_void_at TIMESTAMPTZ,
  countersigned_at TIMESTAMPTZ,
  executed_at TIMESTAMPTZ,
  declined_at TIMESTAMPTZ,
  expires_at TIMESTAMPTZ,
  legal_execution_date DATE,
  sent_file_id TEXT REFERENCES core.files (id) ON DELETE RESTRICT,
  signed_file_id TEXT REFERENCES core.files (id) ON DELETE RESTRICT,
  void_reason TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  invite_error TEXT NOT NULL DEFAULT '',
  pdf_attempts INTEGER NOT NULL DEFAULT 0,
  last_action_by TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
-- One live signing per agreement, as Unified.
CREATE UNIQUE INDEX signings_one_live ON signings (fee_agreement_id)
  WHERE state NOT IN ('executed', 'declined', 'expired', 'voided', 'error');
CREATE INDEX signings_matter ON signings (matter_id, state);

-- Every DocuSeal webhook delivery, deduplicated by submission, event, submitter and time.
CREATE TABLE docuseal_webhook_receipts (
  id TEXT PRIMARY KEY,
  dedupe_key TEXT NOT NULL UNIQUE,
  submission_id TEXT NOT NULL DEFAULT '',
  event_type TEXT NOT NULL DEFAULT '',
  payload JSONB NOT NULL,
  signing_id TEXT REFERENCES signings (id) ON DELETE RESTRICT,
  duplicate_count INTEGER NOT NULL DEFAULT 0,
  verdict TEXT NOT NULL CHECK (verdict IN ('received', 'applied', 'duplicate', 'orphan', 'terminal', 'ignored')),
  received_at TIMESTAMPTZ NOT NULL
);
