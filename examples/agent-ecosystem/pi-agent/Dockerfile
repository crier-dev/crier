FROM node:22-alpine
WORKDIR /srv
COPY package.json .
RUN npm install --no-audit --no-fund 2>/dev/null || true
COPY consumer.mjs .
EXPOSE 9101
CMD ["node", "consumer.mjs"]
