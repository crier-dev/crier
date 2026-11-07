FROM node:22-alpine
WORKDIR /srv
# Real codex CLI for the live path; failed install => canned mode only.
RUN npm install -g @openai/codex --no-audit --no-fund 2>/dev/null || echo 'install failed (offline?) — canned mode only'
# DeepSeek via codex's provider mechanism (owner directive 2026-11-07: no OpenAI
# key needed to prove the harness). wire_api="chat" — DeepSeek serves the
# chat-completions API, not OpenAI's /v1/responses.
RUN mkdir -p /root/.codex && printf '%s\n' \
  'model = "deepseek-chat"' \
  'model_provider = "deepseek"' \
  '' \
  '[model_providers.deepseek]' \
  'name = "DeepSeek"' \
  'base_url = "https://api.deepseek.com/v1"' \
  'env_key = "DEEPSEEK_API_KEY"' \
  'wire_api = "chat"' > /root/.codex/config.toml
COPY consumer/consumer.mjs registration-payloads.json ./
EXPOSE 9104
ENV AGENT_ID=codex HARNESS=codex CODEX_LIVE=0
CMD ["node", "consumer.mjs"]
