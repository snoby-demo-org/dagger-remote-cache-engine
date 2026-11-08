# Cleanup follow-up

The remaining R2 cleanup finding is now fixed in `tests/functional/run.py`:
- `run_container` records each explicit owned named volume before launching Docker.
- `teardown` checks Docker removal exit codes and reads back each exact container, named volume and network with inspect. A still-present resource or unrecognized inspect error is a failure, not proof of absence. It skips already-removed container writes but still checks absence.
- `run` catches cleanup exceptions, sets exit 1, records cleanup_error and writes results after teardown.
- `test_cleanup.py` reproduced four failures before these fixes: nonzero rm, rm-success-but-resource-still-present, omitted named-volume removal, and cleanup failure missing from final evidence. The Python suite is rerun after the fixes.
- Root-owned state directories are retained and explicitly recorded as state_cleanup_error; these are forensic files, not live Docker resources.

Retained counterexample for the earlier S3 false-pass claim: `/data/dcache-dev/artifacts/final-functional/dcache-ft-1794161344079-2a6fd8.json` on ethpow has exit_code 1 after S3-delete failed. Its local log is `agents/final-functional.log`. Current constructor records engine_fixture_args and engine_gc_note; final JSON mirrors them.
