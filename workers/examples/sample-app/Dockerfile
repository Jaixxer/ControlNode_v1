FROM node:22-alpine

WORKDIR /app

COPY package.json ./
COPY index.js ./
COPY public ./public

EXPOSE 3000

CMD ["node", "index.js"]
