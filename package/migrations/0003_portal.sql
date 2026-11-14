-- What the client portal reads: a one-row module state, the payable invoices and matter
-- deposits, and the three published views (client-portal SPEC section 7).

-- Rewritten each time the module loads its settings, so the views answer not_configured
-- without a settings read.
CREATE TABLE module_state (
  id SMALLINT PRIMARY KEY CHECK (id = 1),
  lawpay_ready BOOLEAN NOT NULL,
  trust_ready BOOLEAN NOT NULL,
  docuseal_ready BOOLEAN NOT NULL,
  min_charge_cents BIGINT NOT NULL,
  checked_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE payable_invoices (
  invoice_id TEXT PRIMARY KEY REFERENCES core.invoices (id) ON DELETE RESTRICT,
  matter_id TEXT NOT NULL REFERENCES core.matters (id) ON DELETE RESTRICT,
  destination TEXT NOT NULL CHECK (destination IN ('trust', 'operating')),
  status TEXT NOT NULL CHECK (status IN ('open', 'funded', 'paid', 'void')),
  funded_cents BIGINT NOT NULL DEFAULT 0,
  opened_at TIMESTAMPTZ NOT NULL,
  closed_at TIMESTAMPTZ,
  reopened_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT
);

CREATE TABLE matter_deposits (
  matter_id TEXT PRIMARY KEY REFERENCES core.matters (id) ON DELETE RESTRICT,
  deposits_enabled BOOLEAN NOT NULL DEFAULT true,
  updated_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE VIEW invoice_payability AS
  SELECT r.invoice_id, (r.unpayable_reason = '') AS payable, r.unpayable_reason, r.in_progress_cents
  FROM (
    SELECT p.invoice_id,
      CASE
        WHEN NOT COALESCE(s.lawpay_ready, false) THEN 'not_configured'
        WHEN p.destination = 'trust' AND NOT COALESCE(s.trust_ready, false) THEN 'not_configured'
        WHEN p.status = 'paid' THEN 'zero_balance'
        WHEN p.status = 'funded' OR q.cents > 0 THEN 'in_progress'
        ELSE ''
      END AS unpayable_reason,
      (q.cents + CASE WHEN p.status = 'funded' THEN p.funded_cents ELSE 0 END) AS in_progress_cents
    FROM payable_invoices p
    LEFT JOIN module_state s ON s.id = 1
    CROSS JOIN LATERAL (
      SELECT COALESCE(sum(c.amount_cents), 0)::BIGINT AS cents FROM lawpay_charges c
      WHERE c.invoice_id = p.invoice_id AND c.state IN ('authorized', 'charging', 'charged_unrecorded', 'recorded_pending')
    ) q
    WHERE p.status <> 'void'
  ) r;

CREATE VIEW matter_payability AS
  SELECT d.matter_id,
    (d.deposits_enabled AND COALESCE(s.lawpay_ready, false) AND COALESCE(s.trust_ready, false)) AS trust_deposits_enabled,
    COALESCE(s.min_charge_cents, 0)::BIGINT AS minimum_deposit_cents
  FROM matter_deposits d
  LEFT JOIN module_state s ON s.id = 1;

CREATE VIEW signature_requests AS
  SELECT g.sent_file_id AS file_id, g.id AS signing_request_id, g.matter_id, g.client_contact_id AS signer_contact_id,
    CASE g.state
      WHEN 'sent' THEN 'awaiting_client'
      WHEN 'client_signed' THEN 'client_signed'
      WHEN 'processing_post_execution' THEN 'completed'
      WHEN 'executed' THEN 'completed'
      WHEN 'declined' THEN 'declined'
      WHEN 'expired' THEN 'expired'
      ELSE 'void'
    END AS status,
    extract(epoch FROM g.sent_at)::BIGINT AS sent_at,
    extract(epoch FROM g.expires_at)::BIGINT AS expires_at
  FROM signings g
  WHERE g.sent_file_id IS NOT NULL AND g.state NOT IN ('sending', 'error');
