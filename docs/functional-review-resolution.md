# Functional review resolution

The first acceptance audit is `agents/claude-functional-final.json` (read-only Claude Opus). It was static review, not execution evidence.

| Finding | Resolution |
|---|---|
| Thread-barrier peak was not actual query overlap | Accepted. Mixed driver now records per-query monotonic intervals derived from the measured Docker/CLI query duration and computes sweep-line overlap. A regression distinguishes four serial intervals from four overlapping ones. |
| Host thread activity mislabeled as coordinator debug state | Accepted. Thread activity is separately named; debugstate is read from authenticated `/debug/state`. |
| Failed S3 deletion might still pass | The reported false pass was rejected: Harness.run checks every stage and the actual earlier deletion failure exited 1. Nevertheless scenario_all now also returns False immediately when deletion leaves objects. |
| GC override not recorded | The absence claim was rejected: Harness.__init__ already recorded engine_fixture_args and actual JSON contains it. Added an explicit engine_gc_note explaining the test-only bounded policy and unchanged production settings. |
| Teardown exceptions swallowed | Accepted in mixed driver. Exceptions are recorded, set nonzero exit status, and final evidence is written after teardown. Root-owned forensic state directories can remain intentionally; Docker resource removal is additionally verified by the orchestrator. |

No 96-engine execution or production throughput claim is made. Capacity guard outcomes are NOT_RUN, not PASS.
