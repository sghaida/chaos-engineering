# Admission Policy Test Matrix

This document enumerates test cases for
`ValidatingAdmissionPolicy faultinjection.chaos.sghaida.io`.

The goal of this suite is to **prove and lock in admission-time safety guarantees** for the `FaultInjection` CRD, covering both:

* **schema-level validation (CRD/OpenAPI)** and
* **cross-field invariants enforced by the ValidatingAdmissionPolicy**.

---

## Directory Layout

```text
fi-operator/
└── test/
    └── admission-policy/
        ├── run-tests.sh              # test runner
        ├── sync-expectations.sh      # auto-sync DENY messages
        └── usecases/
            ├── 00-baseline-*.yaml
            ├── 01-*.yaml
            └── ...
```

* All test cases live under `usecases/`
* Each YAML is **self-describing** via header comments

---

## Test Case Conventions

Each YAML under `usecases/` must include:

```yaml
# EXPECT: ALLOW | DENY
# EXPECT_MSG_CONTAINS: <substring> [ || <alternative substring> ]
```

Notes:

* `EXPECT_MSG_CONTAINS` is **required for DENY cases**
* Multiple acceptable messages can be listed using `||`

  * Useful when denial may come from **CRD schema** *or* **admission policy**
* Messages are matched as **substring contains**, not exact equality

---

## How to Run Tests

### Dry-run (recommended)

Exercises **admission only**, without persisting objects:

```bash
FAIL_FAST=1 NS=demo MODE=dry-run ./run-tests.sh
```

* `FAIL_FAST=1` → stop on first failure (tight feedback loop)
* Uses:
  `kubectl apply --server-side --dry-run=server`

### Apply for real (optional)

```bash
NS=demo MODE=apply ./run-tests.sh
```

⚠️ This will create/update CRs in the cluster.

---

## Auto-sync DENY Expectations (New)

When CRD schema or admission messages change, keeping
`# EXPECT_MSG_CONTAINS:` in sync can be tedious.

The helper script below **automates this**.

### Sync expectations from the live API server

```bash
NS=demo ./sync-expectations.sh
```

What it does:

1. Iterates over `usecases/*.yaml`
2. Runs:

   ```bash
   kubectl apply --server-side --dry-run=server
   ```

3. For each `# EXPECT: DENY` case:

   * captures the real error message
   * inserts or updates `# EXPECT_MSG_CONTAINS:`

### Append instead of replace (recommended)

```bash
NS=demo APPEND=1 ./sync-expectations.sh
```

This preserves existing messages and appends alternatives using `||`.

---

## Recommended Workflow

```bash
# 1. Update CRD / admission policy
make install

# 2. Sync DENY expectations from cluster
NS=demo APPEND=1 ./sync-expectations.sh

# 3. Run full admission test suite
FAIL_FAST=1 NS=demo MODE=dry-run ./run-tests.sh
```

This ensures:

* tests always reflect **real cluster behavior**
* regressions are caught immediately
* expectations stay accurate across schema/policy changes

---

## Test Matrix

| Scenario                                  | YAML diff (from baseline)                                | Expected |
| ----------------------------------------- | -------------------------------------------------------- | -------- |
| Baseline INBOUND latency                  | `00-baseline-inbound-latency.yaml`                       | ALLOW    |
| Baseline OUTBOUND abort                   | `00-baseline-outbound-abort.yaml`                        | ALLOW    |
| Baseline stopConditions overall mode      | `00-baseline-stopconditions-overall.yaml`                | ALLOW    |
| Baseline stopConditions per-rule defaults | `00-baseline-stopconditions-perrule-defaults.yaml`       | ALLOW    |
| Missing spec.blastRadius                  | `01-missing-spec-blastradius.yaml`                       | DENY     |
| Missing spec.actions.meshFaults           | `02-missing-spec-actions-meshfaults.yaml`                | DENY     |
| Empty meshFaults list                     | `03-empty-meshfaults-list.yaml`                          | DENY     |
| durationSeconds = 0                       | `04-durationseconds-0.yaml`                              | DENY     |
| maxTrafficPercent = 101                   | `05-maxtrafficpercent-101.yaml`                          | DENY     |
| maxTrafficPercent = -1                    | `06-maxtrafficpercent-1.yaml`                            | DENY     |
| Duplicate meshFault name                  | `07-duplicate-actions-meshfaults-name.yaml`              | DENY     |
| percent = 101                             | `08-percent-101.yaml`                                    | DENY     |
| percent = -1                              | `09-percent-1.yaml`                                      | DENY     |
| percent > blastRadius                     | `10-percent-exceeds-blastradius-maxtrafficpercent.yaml`  | DENY     |
| Missing action.http                       | `11-missing-action-http.yaml`                            | DENY     |
| Missing http.routes                       | `12-missing-http-routes.yaml`                            | DENY     |
| Empty http.routes                         | `13-empty-http-routes.yaml`                              | DENY     |
| Route missing match                       | `14-route-missing-match.yaml`                            | DENY     |
| Match missing uri                         | `15-route-match-has-neither-uriprefix-nor-uriexact.yaml` | DENY     |
| HTTP_LATENCY missing delay                | `16-http-latency-missing-http-delay.yaml`                | DENY     |
| HTTP_ABORT missing abort                  | `18-http-abort-missing-http-abort.yaml`                  | DENY     |
| INBOUND missing virtualServiceRef         | `21-inbound-missing-virtualserviceref-name.yaml`         | DENY     |
| OUTBOUND missing destinationHosts         | `24-outbound-missing-destinationhosts.yaml`              | DENY     |
| stopConditions rules empty                | `32-stopconditions-rules-empty.yaml`                     | DENY     |
| structured metric/query incompatibilities | `43–49-*.yaml`                                           | DENY     |
| compare.op invalid/missing                | `50–51-*.yaml`                                           | DENY     |
| Gap: delay <= timeout allowed             | `90-gap-delay-le-timeout-allowed-by-policy.yaml`         | ALLOW    |
| Gap: spec.cancel allowed                  | `90-gap-cancel-allowed-by-policy.yaml`                   | ALLOW    |

> See individual YAML headers for the exact expected message substrings.

---

## Design Notes

* Some DENY cases are rejected by the **CRD schema** before the admission policy runs.
* This is intentional and acceptable.
* Tests explicitly allow either source of denial when relevant.

This keeps the suite **robust, future-proof, and honest about enforcement boundaries**.
