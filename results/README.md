# Benchmark Results

This folder contains official benchmark results from metal instances.

## Structure

```
results/
├── latest/          # Most recent benchmark results
│   ├── arm64/       # ARM64 Graviton results
│   └── x86/         # x86 Intel results
├── v0.1.0/          # Results for release v0.1.0
├── v0.2.0/          # Results for release v0.2.0
└── ...
```

## Notes

- `latest/` is updated on every Metal benchmark run (manual or release)
- Version folders (e.g., `v0.1.0/`) are only created on release events
- Fast mode (PR) results are shown in the PR summary but not committed

## Running Benchmarks

See the main [README.md](../README.md) for instructions on triggering benchmarks.
