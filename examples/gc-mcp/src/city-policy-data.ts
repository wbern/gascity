// Generated author-verified public city policy snapshot; no raw configuration or prompts.
export const CITY_POLICY = {
  "id": "gc3-city-policy-2026-11-23",
  "profile": "gc3-2026-11-22",
  "host": "clodcod",
  "city": "gc2",
  "label": "GC3",
  "version": "2026-11-23.1",
  "definition_sha256": "104c5491c6c7dc4b1a6cf27a575ca80ace28644916de00442bcdfe0b63f30932",
  "verified_at": "2026-11-23T01:02:03.078600+00:00",
  "dispatch_rules": [
    "For implementation routing, use the configured worker-level selector and preserve the requested level/provider constraints.",
    "On capacity refusal, retain and retry the same request; never silently downgrade or bypass the selector.",
    "Treat routing, readiness and concrete claims separately; a valid configured pool need not have a live worker."
  ],
  "review_rules": [
    "The repository and review context select the owning city/order. Check that order on its execution city; another city's old order is not a fallback.",
    "The claimed formula owns publication and closure. Require matching exact-head GitHub readback; do not synthesize review beads or hand-publish verdicts."
  ],
  "review_lanes": [
    {
      "repository": "wbern/gascity",
      "review_context": "code-review",
      "owner_city": "gc3",
      "owner_order": "gas-city-wbern-pr-review"
    },
    {
      "repository": "wbern/gas-city-infra",
      "review_context": "code-review",
      "owner_city": "gc3",
      "owner_order": "gas-city-infra-pr-review"
    },
    {
      "repository": "wbern/crm",
      "review_context": "code-review",
      "owner_city": "gc3",
      "owner_order": "crm-pr-review"
    },
    {
      "repository": "wbern/crm",
      "review_context": "ux-review",
      "owner_city": "gc3",
      "owner_order": "crm-ux-review"
    }
  ],
  "sources": {
    "dispatch_skill": {
      "repository": "wbern/gas-city-infra",
      "path": "city-template/skills/gc-dispatch/SKILL.md",
      "revision": "bec280951aea1869e2bff7446846b3fe74c99d13",
      "sha256": "884380bc051d4dd3053eda5d5b9ef5dee986effc8cb2472766c083c50c8bdc97"
    },
    "review_skill": {
      "repository": "wbern/gas-city-infra",
      "path": "city-template/packs/gas-city-basics/skills/gas-city-pr-review/SKILL.md",
      "revision": "6aaefb31759046c1a260f4ee1573443a67ec1ec8",
      "sha256": "42822b5c918cc14a8942484f4edce14940c64ed4648688bf5b65b1520e1b933a"
    },
    "review_manifest": {
      "repository": "wbern/gas-city-infra",
      "path": "config/review-lane-ownership.toml",
      "revision": "e63efe5517efbec6d5fed69b561230bb6ebf042b",
      "sha256": "fc871f782d066e59e5467e1dc8fb98cdbd5af7d9ad54d106ccafc4ab1356bee7"
    },
    "selector": {
      "repository": "wbern/gas-city-infra",
      "path": "scripts/worker-level-sling.py",
      "revision": "06c476c2ebf2cdfbb450673a0cb202638953c4e7",
      "sha256": "7a0f5bfbc69710dc4d656b82fa09e3f9909d354194e5a1a3116f2e33b21ec607"
    },
    "routing": {
      "repository": "wbern/gas-city-infra",
      "path": "city-template/config/worker-level-routing-v1.toml",
      "revision": "60968299a2429f538579838ccce5bda27dd766e0",
      "sha256": "30509e19c9ef3695f2baf0f1ca3acee3c8654434083240ebf0db2a57c38da41e"
    }
  },
  "dispatch_verification": {
    "selector": {
      "committed_working_match": false,
      "committed_installed_match": false,
      "working_installed_match": true,
      "installed_sha256": "3b105cb9fc8e7bc6d27e1b39b5bca6c448e275356b68bf18ce9c07a5063a8042",
      "working_sha256": "3b105cb9fc8e7bc6d27e1b39b5bca6c448e275356b68bf18ce9c07a5063a8042"
    },
    "routing": {
      "committed_working_match": false,
      "committed_installed_match": false,
      "working_installed_match": false,
      "installed_sha256": "5b5ceb6bcdcb36a71657d0980f1c609e0d0db3941bec84d45f4e222730f0a1e2",
      "working_sha256": "9b7c185ab33ec934dd985092ce2073293a252f78bf4acfa87d73a31deb8ac9ea"
    }
  },
  "provider_defaults_verified": false
} as const;
