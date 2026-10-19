# Benchmark metric validation boundaries

The benchmark harness checks that `metrics.json` agrees with
`predictions.jsonl` for supported classification and regression metrics.
It rejects non-finite regression predictions/targets, non-finite intermediate
MSE/MAE calculations, and non-finite values in the tolerance comparison.
Numeric strings remain supported for regression.

Metric names ignore ASCII case and surrounding whitespace. Repeated JSON
keys and names that collide after lowercase/whitespace normalization are
rejected before JSON map decoding can discard them. Lookup uses the same
normalization, so Unicode case-fold lookalikes cannot shadow the supported
ASCII names (`accuracy`, `macro_f1`, `mse`, `mae`).

Regression validation fails closed on intermediate overflow, including when
only MAE is reported. Extremely large finite data may therefore require
rescaling even if a different averaging algorithm could produce finite MSE.

These checks establish artifact consistency, not provenance. Predictions and
targets still come from the adapter; metric recomputation does not prove that
they came from the claimed model, dataset rows, training run, or paper.
Offline tests and smoke examples are not full paper reproductions. Source
patch restoration is not a container or filesystem snapshot.

Regression coverage lives in `backend/internal/agent/benchmark_numeric_test.go`:
non-finite values, arithmetic overflow, duplicate/ambiguous metric names,
Unicode lookup consistency, malformed JSON, normal finite inputs, and an
on-disk output directory containing an infinite regression prediction.
