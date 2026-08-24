FROM node:22-alpine
WORKDIR /srv
# Real codex CLI for the live path; failed install => canned mode only.
RUN npm install -g @openai/codex --no-audit --no-fund 2>/dev/null || echo 'install failed (offline?) — canned mode only'
COPY consumer/consumer.mjs .
EXPOSE 9104
ENV AGENT_ID=codex HARNESS=codex CODEX_LIVE=0
CMD ["node", "consumer.mjs"]
