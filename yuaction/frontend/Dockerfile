FROM --platform=$BUILDPLATFORM node:24.18.0-alpine3.23 AS build
WORKDIR /repo/yuaction/frontend
COPY yuaction/frontend/package.json yuaction/frontend/package-lock.json ./
RUN npm ci
COPY frontend/src/core/transcription/ /repo/frontend/src/core/transcription/
COPY yuaction/frontend/ ./
RUN npm run build

FROM nginx:stable-alpine
COPY yuaction/frontend/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /repo/yuaction/frontend/dist /usr/share/nginx/html
EXPOSE 80
