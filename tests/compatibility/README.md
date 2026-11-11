# Compatibility tests

The versioning rules are:

| Contract | Version unit | Compatibility rule |
|---|---|---|
| REST API | `/api/v1` | additive fields; breaking changes require a new path version |
| Plugin protocol | `protocol_major` / `protocol_minor` | major must match; minor is additive |
| Evidence | `patchproof.evidence/v1` | new optional fields only; unknown fields ignored by readers |
| Report | `patchproof.report/v1` | new optional fields only; required fields are stable |
| Policy | `patchproof.io/v1` | unknown fields rejected to prevent silent weakening |
| IR | `patchproof.ir/v1` | additive language-extension fields only |
| Events | `patchproof.<domain>.<action>.v1` | consumers ignore unknown fields and deduplicate by event id |

`TestReportJSONToleratesAdditiveFields` guards forward compatibility for
reports. `TestPolicyParsingRejectsUnknownFields` guards the opposite rule for
policy, where a typo must not silently disable a check. Update this directory's
tests whenever a schema version changes and record the change in
`docs/migration/version-compatibility.md`.
