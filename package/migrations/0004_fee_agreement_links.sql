-- The foreign keys from the DocuSeal records to the core fee agreement. They wait on the
-- core making fee_agreement a record a module can link to (CORE-22), so they sit in their
-- own migration after every one that installs today (package/pending.yaml).
ALTER TABLE agreement_details ADD CONSTRAINT agreement_details_fee_agreement_id_fkey
  FOREIGN KEY (fee_agreement_id) REFERENCES core.fee_agreements (id) ON DELETE RESTRICT;
ALTER TABLE signings ADD CONSTRAINT signings_fee_agreement_id_fkey
  FOREIGN KEY (fee_agreement_id) REFERENCES core.fee_agreements (id) ON DELETE RESTRICT;
