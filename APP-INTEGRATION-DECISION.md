# Burn Workstation and the miner showcase

## What I found

The local workstation checkout is `C:\Users\user\Downloads\burn-upstream-opencode`. Its Git remote is `anomalyco/opencode`, its root package is marked `private`, and the project includes the upstream OpenCode MIT license. The Burn handoff describes a proposed public surface, but says the private app checkout stays private and publication needs a separate decision. It also calls out preserving upstream notices, asset-specific licenses, and credit.

I searched the app, desktop, core, server, and plugin packages for Telegraph routes, miner ID/name, and the miner response field. No current miner integration was present in those packages at inspection time. The workstation therefore cannot yet be presented as an app that uses miner 6149.

## Recommendation

Keep the showcase repo separate from the workstation fork. The showcase is small and focused on miner 6149, its evaluation, and how to test it. Do not copy miner source into the OpenCode fork or publish the full workstation checkout as a shortcut: it has a separate upstream and licensing/audit boundary, and the local handoff keeps that checkout private pending a deliberate release review.

For the current showcase, present **miner + evaluation**. Mention Burn only as a planned consumer if useful, with wording such as: “We are exploring a Burn Workstation integration that would request a wash-trading check when an investigation needs evidence we do not have locally.” Do not say Burn currently calls Telegraph until a working integration is implemented and verified.

## If we build the integration

Keep the app integration thin and explicit:

1. Burn identifies a specific evidence gap and asks the user before triggering a paid miner call.
2. The integration calls Telegraph with the selected Solana mint and records the returned verdict, coverage, timestamp, and evidence in the investigation.
3. Burn labels the miner output as external evidence, preserves `inconclusive`, and does not turn it into a claim of intent or wallet identity.
4. The demo shows a real, reproducible investigation step and its cost. If the integration is included in a public release, publish only the reviewed Burn changes and a redistribution-cleared sample under the separate workstation release process.

This keeps the story accurate: local evidence first, marketplace evidence for a defined gap, with the paid call and its limits visible to the investigator.

