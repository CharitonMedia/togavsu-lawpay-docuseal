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

An admin opens **Administrative > Modules and marketplace > LawPay and DocuSeal** and enters:

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
response. The module reads them from the core on each request. **Money > LawPay**, on the
**LawPay and DocuSeal setup** tab, checks the connection: a LawPay read with the secret key, the trust account, and
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
- Matter screen: **Payments and signing** (client deposits on or off, proposed refunds),
  **Actions > Take payment** and **Actions > Refund**, and the **Fee agreement** panel
  (draft, Send for signature, Resend signing link, Void).
- **Money > LawPay**: the **LawPay reconciliation**, **Fee agreements** and **LawPay and DocuSeal setup** tabs.
- Dashboard widgets: agreements awaiting signature, and open exceptions.
- Help pages are in the manifest under `help:`.

Agents use the module's own API under `/m/lawpay-docuseal/api/`, described by
`/m/lawpay-docuseal/api/openapi.json` (source: `web/openapi.json`).

## Build and test

```
go vet ./...
TOGAVSU_TEST_DATABASE_URL=postgres://postgres:pass@127.0.0.1:5432/togavsu_lawpay_docuseal?sslmode=disable go test -race ./...
docker build -t togavsu-lawpay-docuseal .
```

`TOGAVSU_TEST_DATABASE_URL` is a Postgres superuser URL. Without it the Postgres tests
skip locally and fail in CI. Two suites use it:

- `*_pg_test.go` at the root, one test per MOD-PAY criterion, against an in-process fake
  core (`internal/fakecore`) in the URL's database.
- `acceptance/`, the same criteria against the real core through the module kit's
  `pkg/modulekit/coretest`, which creates and drops its own databases. Each test names
  the core items it needs; while the pinned core (go.mod) doesn't build one, the test
  skips with `needs CORE-NN`. `internal/coreitems` lists those items for the skips only;
  drop an entry when the pin moves to a core that builds it. The install itself runs on
  the pinned core today, with the pending parts held back.

LawPay and DocuSeal are in-memory fakes (`fake/`). No test opens a network connection to
LawPay, DocuSeal or anything else, and every key is built at runtime.

## Packaging and signing

```
lawpay-docuseal package -key signing.key -out lawpay-docuseal.signed.json [-dir package]
```

The key file holds a base64 Ed25519 seed; keep the release key out of the repository. The
command builds with the core's public module kit (`modulekit.BuildInstallable`): it holds
back each part `package/pending.yaml` lists that the pinned core refuses, prints one line
per held-back part with the core item it waits on, runs the core's install check on the
rest, and archives and signs it, so the format, digest and signature are the core's own.
It refuses, listing every refusal, a package the core would refuse once those parts are
out, and prints a line for any pending entry that held nothing back. It prints the key id
and public key to trust in the core. The core's own command builds the same signed file:

```
go run github.com/CharitonMedia/togavsu/cmd/togavsu-module package -installable -dir package -key signing.key -out lawpay-docuseal.signed.json
```

`package/pending.yaml` names each part of the spec the pinned core does not accept yet
and the core item (docs/modules/CORE-CHANGES.md in the core) it waits on: the
`payment_processor`, `inbound_webhooks` and `notifications` fields, the sidebar section,
icon and order, the client tabs `pay` and `sign` and the home tabs' params, the
`fee_agreement` extension, the scopes `books:read`, `mail:send` and `client_access:read`,
and the two trailing migrations that add the foreign keys to core fee agreements
(`0004_fee_agreement_links.sql`, CORE-22) and core payment refunds
(`0005_payment_refund_link.sql`, CORE-24). A part the core accepts stays in the package,
so each returns on its own when its core item lands; migrations are held back whole and
only after every migration that installs, so a firm that installs today takes them later
as an update. The package tests build and sign with a generated key, verify the file and
run the core's check on what it ships, and fail when the core refuses a part
`pending.yaml` doesn't list or when an entry holds nothing back (delete it then).

The image digest in the manifest stays a placeholder until the image is published.

## Core requirements

The module needs the core's module API update (docs/modules/CORE-CHANGES.md in the core):
processor payment grants, refund grants and background file grants delivered as events,
the inbound webhook route, notification kinds, tabs, widgets and published views. Where
it reads a field or calls an operation the core doesn't have yet, docs/SPEC.md says so.
