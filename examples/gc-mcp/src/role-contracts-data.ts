// Generated operator-verified standards snapshot; raw prompts and verification anchors are excluded.
export const ROLE_PROFILE = {
  "definition_sha256": "344fa4455ba51b1b7f078041405b179f8aa7d09c37ddaa0959bee452f1fef067",
  "id": "gc3-2026-11-22",
  "city": "gc2",
  "host": "clodcod",
  "version": "2026-11-22.1",
  "verified_at": "2026-11-22T22:55:29.109Z",
  "contracts": [
    {
      "id": "crm-developer",
      "identity": {
        "agent": "crm/steve",
        "pool": null,
        "pack": null
      },
      "purpose": "CRM developer",
      "standards": [
        "Develop the CRM within current assigned and authorized work; a role label does not create an assignment.",
        "Use the dedicated session worktree, preserving other work.",
        "Handoff mail is context, not work authority; respect explicit holds and current assignment."
      ],
      "verification": {
        "public_fields_verified": true,
        "verified_clause_count": 3,
        "full_template_match": false,
        "installed_template_sha256": "818408b727c1df6c9125122b93a7aeea5d7c23d91437a9e8319a0bd9aa6dd3ba",
        "basis": "selected resolved template and verified clauses; not full effective instructions"
      },
      "sources": [
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/agents/steve/agent.toml",
          "revision": "0dbfca0dee5b9f0b79dd6f78082741f3a2aa023d",
          "sha256": "cec756392516cd99b131b7c7b0da9e9247d7205d0cc17756d05e502e78c3539b"
        },
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/agents/steve/prompt.template.md",
          "revision": "74713e74a29f3044edf5315903679e49065a2a93",
          "sha256": "f9f6ec51f151dcd97d59d40e0e9c628721352c3d627ecbf38b1c7c139bd28940"
        }
      ]
    },
    {
      "id": "crm-architecture",
      "identity": {
        "agent": "crm/architect",
        "pool": null,
        "pack": null
      },
      "purpose": "CRM architecture and integrity",
      "standards": [
        "Provide structural design authority for the CRM; use current assigned scope to determine the next action.",
        "Account for authentication boundaries in architecture decisions.",
        "Handoff mail supplies context rather than creating a work assignment; preserve holds."
      ],
      "verification": {
        "public_fields_verified": true,
        "verified_clause_count": 3,
        "full_template_match": false,
        "installed_template_sha256": "93bb7fbe454d04c44c882df2b470b93255ddf75886e00443bfa6b12d42b099bd",
        "basis": "selected resolved template and verified clauses; not full effective instructions"
      },
      "sources": [
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/agents/architect/agent.toml",
          "revision": "0dbfca0dee5b9f0b79dd6f78082741f3a2aa023d",
          "sha256": "5f35fa4467e90a0db467b71209417959655579fc81dff6a7262f2ad162654842"
        },
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/agents/architect/prompt.template.md",
          "revision": "74713e74a29f3044edf5315903679e49065a2a93",
          "sha256": "211c106f82887f18ddedd6fad3caff7c7adc66697c6d6d9ec2fa7b839237f214"
        }
      ]
    },
    {
      "id": "infra-operations",
      "identity": {
        "agent": "gas-city-infra/devops",
        "pool": null,
        "pack": null
      },
      "purpose": "Gas City infrastructure operations",
      "standards": [
        "Act as the hands-on infrastructure and operations collaborator within assigned authority.",
        "Own qualified session-reliability investigations; source changes and deployment remain separate evidence.",
        "Handle remote workstation deployment through the approved operational path and existing authorization."
      ],
      "verification": {
        "public_fields_verified": true,
        "verified_clause_count": 3,
        "full_template_match": false,
        "installed_template_sha256": "8ea8fe79d99f50d9e20741eb70102fcf44b369872e7633b4a732873dfcb6f0d3",
        "basis": "selected resolved template and verified clauses; not full effective instructions"
      },
      "sources": [
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/agents/devops/agent.toml",
          "revision": "0dbfca0dee5b9f0b79dd6f78082741f3a2aa023d",
          "sha256": "734dc4790053ae349c439cfe7e9afc4848cf622c11057f4a027d18f79aadf4a2"
        },
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/agents/devops/prompt.template.md",
          "revision": "74713e74a29f3044edf5315903679e49065a2a93",
          "sha256": "461252f9044e0ac362ee73954e46422cc06ae521f1fd409fda0964e7513a2d52"
        }
      ]
    },
    {
      "id": "crm-code-review",
      "identity": {
        "agent": null,
        "pool": "crm/gastown.reviewer",
        "pack": "gastown"
      },
      "purpose": "Exact-head read-only CRM review",
      "standards": [
        "Review one exact PR head as a bounded read-only reviewer, not an implementation or city-operations worker.",
        "Pin the assigned head and scope; run the claimed review formula in order.",
        "Publish through the guarded named path and verify readback; a stored opinion is not publication.",
        "Do not modify the checkout, publish statuses directly, close the tracker or perform unrelated administration."
      ],
      "verification": {
        "public_fields_verified": true,
        "verified_clause_count": 4,
        "full_template_match": true,
        "installed_template_sha256": "c27e716a8ab99767347debbd051ad2bf444bb1ef01cdea820116496d9f1a4363",
        "basis": "selected resolved template and verified clauses; not full effective instructions"
      },
      "sources": [
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/packs/crm-gastown-lite/agents/reviewer/agent.toml",
          "revision": "2c48469205c470151235d2a5ab0a399c50df653c",
          "sha256": "c4665391191362c6b5b28be78daf8f53e4eea6e298ec43c0fa06677f7386223c"
        },
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/packs/crm-gastown-lite/agents/reviewer/prompt.template.md",
          "revision": "24b7909ca8981f2b3199018f01ddc010c0a7362d",
          "sha256": "c27e716a8ab99767347debbd051ad2bf444bb1ef01cdea820116496d9f1a4363"
        }
      ]
    },
    {
      "id": "gc-implementation-worker",
      "identity": {
        "agent": "gas-city-wbern/pool.worker-default-codex",
        "pool": null,
        "pack": "pool"
      },
      "purpose": "Scoped implementation worker",
      "standards": [
        "Work one claimed bead to verified handoff or a concrete blocker, then drain through the configured workflow.",
        "Do not spawn another core worker or silently change the requested model/provider level.",
        "Follow the configured claim and formula steps; keep startup and model-visible evidence bounded."
      ],
      "verification": {
        "public_fields_verified": true,
        "verified_clause_count": 3,
        "full_template_match": true,
        "installed_template_sha256": "21603174f099da2b067c224ba3442ad746d3d34fadc4e1cb89316e4b86145a05",
        "basis": "selected resolved template and verified clauses; not full effective instructions"
      },
      "sources": [
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/packs/polecat-base/agents/worker-default-codex/agent.toml",
          "revision": "60968299a2429f538579838ccce5bda27dd766e0",
          "sha256": "521a57a07175883b74538d12f8b60cb89e059ecc38a8f0a07e2cc56f65a17933"
        },
        {
          "repository": "wbern/gas-city-infra",
          "path": "city-template/packs/polecat-base/agents/codex-polecat/prompt.template.md",
          "revision": "60968299a2429f538579838ccce5bda27dd766e0",
          "sha256": "21603174f099da2b067c224ba3442ad746d3d34fadc4e1cb89316e4b86145a05"
        }
      ]
    }
  ]
} as const;
