-- LawPay records. Every core id is a foreign key with ON DELETE RESTRICT (the one to a core
-- payment refund in 0005_payment_refund_link.sql); no table holds a copy of a core record
-- (no names, emails, phones or addresses).

-- Core events the module has handled, so each event id is handled once.
CREATE TABLE received_events (
  event_id TEXT PRIMARY KEY,
  action TEXT NOT NULL,
  record_id TEXT NOT NULL,
  received_at TIMESTAMPTZ NOT NULL
);

-- Processor and refund grants the core delivered on payment.processor_authorized and
-- payment.refund_authorized. payment_id has no foreign key: the core payment doesn't exist
-- until createPayment binds the reserved id.
CREATE TABLE processor_grants (
  initialization_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('payment', 'refund')),
  payment_id TEXT NOT NULL,
  refund_id TEXT,
  grant_credential TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL
);

-- One row per LawPay charge, from the pay intent to recorded or failed.
CREATE TABLE lawpay_charges (
  id TEXT PRIMARY KEY,
  matter_id TEXT NOT NULL REFERENCES core.matters (id) ON DELETE RESTRICT,
  contact_id TEXT NOT NULL REFERENCES core.contacts (id) ON DELETE RESTRICT,
  invoice_id TEXT REFERENCES core.invoices (id) ON DELETE RESTRICT,
  payment_id TEXT REFERENCES core.payments (id) ON DELETE RESTRICT,
  reserved_payment_id TEXT,
  initialization_id TEXT UNIQUE,
  replenishment_request_id TEXT,
  purpose TEXT NOT NULL CHECK (purpose IN ('trust_deposit', 'invoice_pay', 'staff_charge')),
  channel TEXT NOT NULL CHECK (channel IN ('portal', 'staff')),
  destination TEXT NOT NULL CHECK (destination IN ('operating', 'trust')),
  method TEXT CHECK (method IN ('card', 'ach')),
  amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
  lawpay_charge_id TEXT UNIQUE,
  lawpay_status TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('intent', 'authorized', 'charging', 'declined', 'charged_unrecorded',
    'recorded_pending', 'recorded_completed', 'failed', 'expired')),
  failure_code TEXT NOT NULL DEFAULT '',
  failure_message TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  initiated_by TEXT NOT NULL REFERENCES core.users (id) ON DELETE RESTRICT,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX lawpay_charges_state ON lawpay_charges (state);
CREATE INDEX lawpay_charges_matter ON lawpay_charges (matter_id);
CREATE INDEX lawpay_charges_payment ON lawpay_charges (payment_id);
-- Card attempts per client per window.
CREATE INDEX lawpay_charges_attempts ON lawpay_charges (contact_id, channel, created_at);

-- One row per refund of a LawPay charge.
CREATE TABLE lawpay_refunds (
  id TEXT PRIMARY KEY,
  charge_id TEXT NOT NULL REFERENCES lawpay_charges (id) ON DELETE RESTRICT,
  payment_id TEXT NOT NULL REFERENCES core.payments (id) ON DELETE RESTRICT,
  -- Its foreign key to core.payment_refunds is in 0005_payment_refund_link.sql.
  core_refund_id TEXT,
  lawpay_refund_id TEXT UNIQUE,
  amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
  reason TEXT NOT NULL DEFAULT '',
  origin TEXT NOT NULL CHECK (origin IN ('firm', 'lawpay_dashboard', 'no_go')),
  state TEXT NOT NULL CHECK (state IN ('proposed', 'core_pending', 'processor_sent', 'processor_done', 'posted',
    'failed', 'external_detected', 'dismissed')),
  failure_message TEXT NOT NULL DEFAULT '',
  source_event_id TEXT UNIQUE,
  requested_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  attempts INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX lawpay_refunds_charge ON lawpay_refunds (charge_id);
CREATE INDEX lawpay_refunds_state ON lawpay_refunds (state);

-- Every LawPay webhook delivery, deduplicated by the SHA-256 of its raw body.
CREATE TABLE lawpay_webhook_receipts (
  id TEXT PRIMARY KEY,
  received_at TIMESTAMPTZ NOT NULL,
  dedupe_key TEXT NOT NULL UNIQUE,
  transaction_id TEXT NOT NULL DEFAULT '',
  event_type TEXT NOT NULL DEFAULT '',
  payload JSONB NOT NULL,
  duplicate_count INTEGER NOT NULL DEFAULT 0,
  verdict TEXT NOT NULL CHECK (verdict IN ('received', 'applied', 'duplicate', 'malformed', 'unverified',
    'unknown_transaction', 'ignored')),
  charge_id TEXT REFERENCES lawpay_charges (id) ON DELETE RESTRICT,
  refund_id TEXT REFERENCES lawpay_refunds (id) ON DELETE RESTRICT
);

-- Anything that needs a person. One open row per kind and record.
CREATE TABLE exceptions (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('charged_unrecorded', 'external_refund', 'unknown_transaction',
    'account_mismatch', 'amount_mismatch', 'refund_unposted', 'manual_refund_needed', 'pending_ach_stale',
    'signed_pdf_failed', 'docuseal_orphan', 'signing_error')),
  matter_id TEXT REFERENCES core.matters (id) ON DELETE RESTRICT,
  charge_id TEXT REFERENCES lawpay_charges (id) ON DELETE RESTRICT,
  refund_id TEXT REFERENCES lawpay_refunds (id) ON DELETE RESTRICT,
  signing_id TEXT,
  transaction_id TEXT NOT NULL DEFAULT '',
  amount_cents BIGINT NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('open', 'resolved')),
  resolved_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  resolved_at TIMESTAMPTZ,
  resolution_note TEXT NOT NULL DEFAULT '',
  notified_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX exceptions_one_open ON exceptions (kind, COALESCE(charge_id, ''), COALESCE(refund_id, ''),
  COALESCE(signing_id, ''), transaction_id) WHERE status = 'open';
CREATE INDEX exceptions_status ON exceptions (status, kind);
