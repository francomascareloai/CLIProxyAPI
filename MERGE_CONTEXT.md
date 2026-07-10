# Merge Context — fork franco ← upstream/main (v7.2.58)

Worktree: `/home/franc/projetos/cliproxy-merge-v7258`
Merge: `franco/usage-persistence-v6.8.4` (v6.8.51) ← `upstream/main` (v7.2.58, 1016 commits).

## Customizações LOCAIS a PRESERVAR (trabalho do Franco; upstream não tem OU removeu)

1. **Usage persistence** — `internal/usage/{hmac,journal,persist,persistence,logger_plugin}.go` + `internal/runtime/executor/usage_helpers.go`. Persistência de stats de uso (HMAC + ring buffer + journal + dashboard). **O UPSTREAM REMOVEU usage tracking** (commit `18bb9c31 chore: remove usage tracking and logging functionality`). O Franco MANTÉM. Nos executors/handlers: se o upstream removeu as chamadas de gravação de usage, **RE-ADICIONE** as chamadas locais (import `internal/usage`, chamar a persistência). O `sdk/cliproxy/usage/manager.go` local tem a lógica de persistência — preserve.
2. **Minimax provider** — `internal/auth/minimax/`, `internal/cmd/minimax_login.go` (flag `--minimax-login`), `internal/runtime/executor/minimax_executor.go`, `internal/thinking/provider/minimax/`, `sdk/auth/minimax.go`. Provider próprio do Franco. Preservar e integrar (registrar em thinking_providers/registry se o upstream espera registro explícito).
3. **Codex metrics** — `internal/runtime/executor/codex_metrics.go`, `codex_common.go`, `internal/api/server_codex_metrics_test.go`. Métricas de codex. Preservar.

## Mudanças do UPSTREAM a PEGAR (v7.2.58)

1. **xAI/Grok nativo** — `internal/auth/xai/`, `internal/cmd/xai_login.go` (`--xai-login`), `internal/runtime/executor/xai_executor.go`, `internal/signature/grok_validation.go`. NOVO provider OAuth. Pegar tudo.
2. **GPT-5.6 models**, registry atualizado (já resolvido).
3. **Refatoração** — executor utilities movidos para `internal/runtime/executor/helps/`. `thinking_providers.go` → `helps/thinking_providers.go`.
4. **gemini-cli translator REMOVIDO** do upstream (local mantém — já resolvido).

## Regras de resolução

- Remova TODOS os markers `<<<<<<<` `=======` `>>>>>>>`.
- Preserve a intenção de AMBOS os lados: customização local (usage/minimax/codex-metrics) + melhorias upstream (xAI/GPT-5.6/refatoração).
- **NÃO rode `git add`/`git commit`** — só edite os arquivos; o orquestrador commita no final.
- Resultado deve ser Go válido (compilável). Mantenha imports consistentes.
- Use `Read(offset, limit)` para ver só os hunks de conflito — **NUNCA leia arquivos inteiros >800 linhas**. Localize conflitos com `grep -n '^<<<<<<<' <file>` primeiro.
- `cd /home/franc/projetos/cliproxy-merge-v7258` antes de editar.
- Após resolver seus arquivos, rode `grep -rn '^<<<<<<<\|^=======$\|^>>>>>>>' <seus_arquivos>` para confirmar 0 markers restantes.
