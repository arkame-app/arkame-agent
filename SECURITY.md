# Security Policy

The agent runs with high privileges on other people's servers and holds their
bucket credentials, so security reports are taken seriously and handled before
anything else.

## Reporting a vulnerability

Please **do not open a public issue**. Email **contato@arkame.app** with
"SECURITY" in the subject and include:

- what you found and where (file, function, or command);
- steps to reproduce, or a proof of concept;
- the agent version (`arkame-agent version`) and OS;
- what an attacker could do with it, as you see it.

Write in English, Portuguese or Spanish.

## What to expect

Arkame is run by a very small team, so these are honest targets, not an SLA:

- an acknowledgment within **3 business days**;
- a first assessment (whether we can reproduce it and how severe we think it is)
  within **10 business days**;
- updates at least every two weeks while a fix is in progress.

Once a fix is released, we will credit you in the release notes if you want to
be credited. Please give us a reasonable time to ship a fix before disclosing
publicly; we aim for **90 days** at most and will tell you if we need longer and
why.

There is no bug bounty program.

## Scope

In scope:

- this repository: the agent, `install.sh`, `install.ps1`, the Windows `setup`
  command;
- the release pipeline (`.goreleaser.yaml`, `.github/workflows`) and the
  published artifacts and container image.

Issues in the Arkame panel (`save.arkame.app`) or website (`arkame.app`) can be
reported to the same address, even though that code is not public.

Out of scope: findings that require an already compromised host or root access
on the server where the agent runs, and reports from automated scanners without
a demonstrated impact.

## Supported versions

Only the latest release receives security fixes. The agent does not update
itself: updating means re-running the install command from the panel (Docker:
`--pull always` fetches the new image).

## Verifying releases

Release checksums are signed with Sigstore cosign; see
[Verify releases](README.md#verify-releases-cosign) in the README.
