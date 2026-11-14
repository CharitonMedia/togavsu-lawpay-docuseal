# LawPay and DocuSeal for Togavsu

A Togavsu module by Chariton Media. Free, licensed the same as Togavsu (see LICENSE).

It does two things:

- **LawPay payments.** Clients pay invoices and make trust deposits from the client
  portal, and staff take card or ACH payments on a matter. Each LawPay charge becomes a
  core payment into the operating or trust account; refunds, full or partial, become
  core payment refunds. A reconciliation tab lists every LawPay transaction and every
  exception that needs a person.
- **DocuSeal fee agreements.** Staff draft a fee agreement on the matter and send it for
  signature. The client signs in the portal, the firm countersigns, and the executed PDF
  is filed on the matter and shared with the client.

Money never moves outside the core. The module runs in its own container, talks to the
core only through the core API, and keeps its own records in its own schema, tied to core
records by `ON DELETE RESTRICT` foreign keys. The full design is in [docs/SPEC.md](docs/SPEC.md).

## Setup

An admin opens **Admin > Modules > LawPay and DocuSeal > Settings** and enters:

| Setting | What it is |
| --- | --- |
| LawPay secret key (secret) | The firm's LawPay API secret key. |
| LawPay public key | For the hosted card and bank fields. |
| LawPay operating account id, trust account id | The two LawPay deposit accounts. |
| Togavsu trust account id | The core trust account LawPay trust deposits post to. It must be active. |
| LawPay API address | Default `https://api.8am.com/v1`. |
| LawPay hosted fields script address | Default `https://cdn.affinipay.com/hostedfields/1.5.3/fieldGen_1.5.3.js`. |
| LawPay webhook URL token (secret) | Any long random value; it goes in the LawPay webhook URL. |
| DocuSeal address, API key (secret), template id | The firm's DocuSeal and its fee agreement template. |
| DocuSeal webhook token (secret) | The `X-Webhook-Token` value set in DocuSeal. |
| Firm signer name and email | Who countersigns for the firm. |

Tunables, all with defaults: smallest and largest charge (100 and 5,000,000 cents),
invoice payment destination (trust), pay page minutes (30), card attempts per window (3)
and window minutes (10), sweep minutes (60), retries before an exception (3), pending ACH
check days (14), and days before an unsigned agreement expires (0, never).

Secrets are never stored in the module's tables, written to its logs or returned in a
response. The module reads them from the core on each request. **Module tabs > LawPay and
DocuSeal setup** checks the connection: a LawPay read with the secret key, the trust account, and
the DocuSeal template.

### Webhooks

Register these with each service (the setup tab shows them with the firm's address):

- LawPay: `https://<your Togavsu address>/hooks/lawpay-docuseal/lawpay?token=<LawPay webhook URL token>`
- DocuSeal: `https://<your Togavsu address>/hooks/lawpay-docuseal/docuseal`, with the
  header `X-Webhook-Token: <DocuSeal webhook token>`

The core forwards POSTs only. The module checks the token in constant time, stores each
delivery once, and confirms every LawPay webhook with a live LawPay read before it changes
anything. Core events to `/events` are checked by HMAC-SHA256 with a five-minute skew
limit, and each event id is handled once.

## Where things are

- Client portal: **Pay** (invoice or trust deposit) and **Sign** (fee agreement).
- Matter: **Payments and signing** (Take payment, Refund, client deposits on or off) and
  **Fee agreement** (draft, Send for signature, Resend signing link, Void).
- Module tabs: **Fee agreements**, **LawPay reconciliation**, **LawPay and DocuSeal setup**.
- Dashboard widgets: agreements awaiting signature, and open exceptions.
- Help pages are in the manifest under `help:`.

Agents use the module's own API under `/m/lawpay-docuseal/api/`, described by
`/m/lawpay-docuseal/api/openapi.json` (source: `web/openapi.json`).

## Build and test

```
go vet ./...
TOGAVSU_TEST_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/dbname?sslmode=disable go test ./...
docker build -t togavsu-lawpay-docuseal .
```

The acceptance tests (`*_pg_test.go`, one per MOD-PAY criterion) run against an
in-process fake core (`internal/fakecore`) and in-memory LawPay and DocuSeal fakes
(`fake/`). No test opens a network connection to LawPay, DocuSeal or anything else, and
every key is built at runtime. Without `TOGAVSU_TEST_DATABASE_URL` the Postgres tests skip
locally and fail in CI.

## Packaging and signing

`lawpay-docuseal package` builds the signed package the core installs. The package
format, digest and signature belong to the core, and this module doesn't copy them, so
packaging goes through the `Packager` interface in `module.go`. Until the core publishes a
public packaging and signing package or CLI, the command uses `PendingPackager` and
stops with a message saying so. Swapping in the core's packager is a one-line change in
`cmd/lawpay-docuseal/main.go`.

## Core requirements

The module needs the core's module API update (docs/modules/CORE-CHANGES.md in the core):
processor payment grants, refund grants and background file grants delivered as events,
the inbound webhook route, notification kinds, tabs, widgets and published views. Where
it reads a field or calls an operation the core doesn't have yet, docs/SPEC.md says so.
