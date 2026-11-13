FROM alpine:latest

RUN apk add --no-cache \
    mpd \
    mpc \
    alsa-utils \
    pulseaudio-utils \
    sox \
    lame \
    flac \
    curl \
    tzdata

RUN mkdir -p /var/lib/mpd/playlists /music /var/log/mpd /run/mpd && \
    touch /var/lib/mpd/database /var/lib/mpd/state /var/lib/mpd/sticker.sql && \
    ln -sfn /music /rawmusic

COPY config/mpd.conf /etc/mpd.conf
COPY docker/mpd/aurabox-dsp-pipe /usr/local/bin/aurabox-dsp-pipe
COPY docker/mpd/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/aurabox-dsp-pipe

EXPOSE 6600 4082

ENTRYPOINT ["/entrypoint.sh"]
