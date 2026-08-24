FROM node:22-alpine
WORKDIR /srv
# Real claude CLI for the live path; a failed install must NOT mask as a
# working binary, but the container still boots for canned mode.
RUN npm install -g @anthropic-ai/claude-code --no-audit --no-fund 2>/dev/null || echo 'install failed (offline?) — canned mode only'
COPY consumer/consumer.mjs .
EXPOSE 9103
ENV AGENT_ID=claude-code HARNESS=claude-code CLAUDE_CODE_LIVE=0
CMD ["node", "consumer.mjs"]
