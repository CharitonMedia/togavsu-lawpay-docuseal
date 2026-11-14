-- The foreign key from a LawPay refund to its core payment refund. It waits on the core's
-- payment refunds (CORE-24), linkable as payment_refund (CORE-22), so it sits in its own
-- migration after every one that installs today (package/pending.yaml).
ALTER TABLE lawpay_refunds ADD CONSTRAINT lawpay_refunds_core_refund_id_fkey
  FOREIGN KEY (core_refund_id) REFERENCES core.payment_refunds (id) ON DELETE RESTRICT;
