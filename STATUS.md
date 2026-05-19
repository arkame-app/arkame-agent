# Arkame Agent — Status

**Status:** scaffold estrutural. Compila, todas as peças arquiteturais posicionadas. Sync engine e integrações reais com o painel ainda não implementadas.

> **Trabalho ativo está no painel** (`~/hugo-projects/arkame/`). Para o status global do produto, ver `~/hugo-projects/arkame/STATUS.md`.

## Próxima integração esperada

Quando o painel terminar Auth + API routes (Sprint 3 fecha com Stripe E2E, Sprint 4 emails, depois entra **Sprint 5: agent contract**), este agent precisa:

1. Casar `internal/api/types.go` com as routes em `apps/save/src/app/api/agents/...`
2. Implementar enrollment Ed25519 + mTLS handshake
3. Implementar walker + hasher + S3 probe (já tem stub)
4. Implementar reporter de metadata (filenames, paths, sizes, hashes, version_ids) — **nunca** conteúdo

## Veja também

- `README.md` aqui — arquitetura completa, estrutura de código, "o que falta"
- `~/hugo-projects/arkame/STATUS.md` — estado do painel, sprints, como retomar
- `~/hugo-projects/arkame/PLAN.md` — histórico de decisões
