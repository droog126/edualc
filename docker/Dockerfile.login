FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium \
    xvfb \
    x11vnc \
    novnc \
    websockify \
    openbox \
    tzdata \
    locales \
    jq \
    ca-certificates \
    fonts-dejavu-core \
    && for loc in en_US.UTF-8 en_GB.UTF-8 en_CA.UTF-8 en_AU.UTF-8 en_NZ.UTF-8 en_IE.UTF-8 en_SG.UTF-8 en_IN.UTF-8 ja_JP.UTF-8 ko_KR.UTF-8 zh_TW.UTF-8 de_DE.UTF-8 de_CH.UTF-8 fr_FR.UTF-8 nl_NL.UTF-8 it_IT.UTF-8 es_ES.UTF-8 pt_BR.UTF-8 sv_SE.UTF-8 pl_PL.UTF-8; do \
         sed -i "s/^# *${loc}/${loc}/" /etc/locale.gen; \
       done \
    && locale-gen \
    && rm -rf /var/lib/apt/lists/*

COPY docker/login-entrypoint.sh /usr/local/bin/login-entrypoint.sh
RUN chmod +x /usr/local/bin/login-entrypoint.sh
EXPOSE 6080
ENTRYPOINT ["/usr/local/bin/login-entrypoint.sh"]
