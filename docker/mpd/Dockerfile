FROM alpine:latest

RUN apk add --no-cache \
    mpd \
    mpc \
    alsa-utils \
    lame \
    flac \
    curl \
    tzdata

RUN mkdir -p /var/lib/mpd/playlists /music /var/log/mpd && \
    touch /var/lib/mpd/database /var/lib/mpd/state /var/lib/mpd/sticker.sql

COPY config/mpd.conf /etc/mpd.conf

EXPOSE 6600 4082

CMD ["mpd", "--no-daemon", "--stdout", "/etc/mpd.conf"]

