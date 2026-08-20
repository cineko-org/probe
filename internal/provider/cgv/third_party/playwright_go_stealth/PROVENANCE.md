# Vendored stealth scripts

The files in this directory are repository-owned copies derived from
`jonfriesen/playwright-go-stealth` commit `b2483b64ee2ae3578a92129457b1c1fc406d324e`. They are embedded JavaScript
inputs, not Go module dependencies, so dependency tooling does not update or verify them automatically.

| File | Upstream SHA-256 | Local policy |
| --- | --- | --- |
| `stealth.min.js` | `375dd3a300f31a6e95a429e16ba1920dc2b7645a454662e851e74ab1f157a557` | Exact upstream copy |
| `chrome_stealth.js` | `573ab09fc19fd780498c697e290c8a6c769f517a6fae8f19ce0417d913564f70` | Reviewed local patch; see below |

Local policy permits passive browser-surface normalization only. Challenge/CAPTCHA clicking, event trust spoofing,
console replacement, credential handling, and arbitrary navigation are intentionally removed. Any refresh must:

1. record the upstream repository commit and source-file checksum in the change description;
2. preserve the no-challenge-automation policy;
3. update browser identity tests and run the real Chromium test suite;
4. review the complete generated diff before replacing either script.
