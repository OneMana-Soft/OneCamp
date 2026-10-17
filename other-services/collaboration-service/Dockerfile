FROM node:18-alpine

WORKDIR /app

# PINNED, as the web image's is. This was `npm install -g pnpm`, whatever pnpm
# was newest on the day of the build (12.14.0 on 17 Oct 2026), so two installs
# a day apart could build with different package managers, and a rebuild
# reused whichever one the layer cache held. The first install of the v2.73.0
# release proof died here (pnpm install, exit 139) and its rerun reused that
# layer. 9.15.4 is the version the web app pins in package.json.
RUN corepack enable && corepack prepare pnpm@9.15.4 --activate

COPY package.json pnpm-lock.yaml ./

RUN pnpm install --frozen-lockfile

COPY . .

CMD ["node", "index.js"]
