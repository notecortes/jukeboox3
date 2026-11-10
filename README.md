# AuraBox — Hi-Fi Audio Jukebox & Embedded Linux Music Player

**AuraBox** es un sistema integral, ultrarrápido y de bajo consumo (<50MB de RAM) diseñado para transformar una **Raspberry Pi (3B+, 4 o 5)** o cualquier máquina Linux en un reproductor de audio audiófilo y Jukebox inteligente.

Cuenta con una **interfaz PWA nativa en HTML5, CSS3 y JavaScript vanilla** (sin Node.js, Angular, React ni Tailwind), diseñada con ergonomía **Thumb-First** (control con una sola mano desde smartphone), un motor de recomendación **Smart Mix** con fallback offline, sincronización de letras en tiempo real vía **LRCLIB**, **283 emisoras de radio web Hi-Fi** (incluidas 50 emisoras españolas), protección acústica de volumen y conmutación en caliente entre Jack 3.5mm, DAC USB e I2S HAT.

---

## 🚀 Arquitectura y Puertos (Docker)

La aplicación está totalmente containerizada mediante `docker-compose.yml` y utiliza estrictamente puertos en el rango **4000** para evitar colisiones:

| Puerto | Protocolo | Descripción |
|---|---|---|
| **4080** | HTTP | Interfaz Web Principal PWA, API REST, flujo SSE y redirección opcional a HTTPS |
| **4443** | HTTPS | Interfaz Web Segura TLS 1.2+ con CA local (`/api/security/ca.crt`) y proxy `/api/stream` |
| **4081** | HTTP | Métricas del sistema y verificación de salud (`/health`, `/metrics`) |
| **4082** | HTTP | Transmisión directa MP3 320kbps de MPD (usada cuando se navega por HTTP) |
| **4600** | TCP | Puerto de control MPD mapeado (aislado del `6600` del host) |

### Puntos de Montaje de Audio y Almacenamiento
- `/dev/snd` + `c 116:* rmw`: Acceso directo al bus de sonido ALSA del host y detección en caliente de DACs USB.
- `/music/local`: Música almacenada en la tarjeta microSD o disco local.
- `/music/nas`: Recursos de red montados automáticamente vía SMB/CIFS o NFS.
- `/music/usb`: Pendrives y discos duros USB extraíbles (`udev` automount).
- `./data` → `/app/data`: Base de datos SQLite (`aurabox.db`), caché de carátulas (`covers/`) y certificados TLS locales (`certs/`).
- `./config/mpd.conf`: Configuración optimizada del motor MPD montada en solo lectura.

---

## 📱 Ergonomía Móvil y UX Thumb-First

Optimizada para smartphones (360px a 430px) y controlable con el pulgar, incluyendo soporte nativo para el botón **Atrás** del dispositivo:

1. **Barra de Navegación Inferior (Dock):** 5 secciones clave (`Biblioteca`, `Buscar`, `Mixes`, `Cola`, `Ajustes`) con blancos táctiles ≥ 48px.
2. **Vistas de Biblioteca Completa:** `Añadido recientemente`, `Álbumes`, `Artistas`, `Favoritas (♥)`, `Explorador de carpetas` (Local / NAS / USB) y catálogo de `Radios Web`.
3. **Mini-Player Persistente Flotante:**
   - Dockeado inmediatamente encima del menú inferior y visible si hay reproducción activa.
   - **Tap / Swipe Up:** Abre **Now Playing** a pantalla completa.
   - **Swipe Horizontal:** Salta de canción (siguiente/anterior) con feedback optimista.
4. **Now Playing Envolvente:**
   - Carátula en alta resolución con fondo dinámico desenfocado e insignia técnica Hi-Fi (`FLAC`, `MP3 320kbps`, etc.).
   - **Swipe Down:** Vuelve a la vista previa.
   - **Letras Sincronizadas (LRCLIB):** Desplazamiento automático línea por línea en 1 toque; permite pulsar cualquier verso para saltar a ese segundo.
5. **Índice Alfabético Vertical (A-Z Slider):** Salto instantáneo en listados largos de artistas y álbumes.
6. **Salvaguarda de Volumen:** Deslizador suave, botones directos `+` / `-` y límite de ganancia máxima configurable (por defecto 85%).
7. **Cola Jukebox Interactiva:** Reordenación táctil **Drag & Drop**, eliminación por **Swipe lateral** y barajado anti-repetición.
8. **MediaSession API & Multi-Dispositivo:** Controles en pantalla de bloqueo + sincronización en tiempo real (<50ms) por **SSE** entre todos los móviles conectados.
9. **PWA Resiliente (`sw.js`):** Estrategia Network-First sin interceptar flujos de audio/SSE ni peticiones con cabecera `Range`, con pantalla de recuperación automática si cambian los puertos de acceso.

---

## ⚡ Motor "Smart Mix" de Artistas Locales

Al seleccionar **«Crear Smart Mix de [Artista]»**:
1. **Consulta Online:** Consulta la API abierta de MusicBrainz / ListenBrainz para descubrir artistas afines.
2. **Fallback Offline Inteligente:** Si no hay internet, cruza en SQLite géneros coincidentes, época (±10 años) y tempo (BPM).
3. **Filtrado contra Almacenamiento:** Limita la selección a canciones presentes en `/music/local`, `/music/nas` o `/music/usb`.
4. **Mezcla Balanceada y Armónica:** **40%** del artista semilla + **60%** de artistas afines, intercalados para no repetir artista de forma consecutiva.

---

## 📻 Radios Web Hi-Fi y Estabilidad 24/7

- **283 emisoras preindexadas de serie** (con filtro directo **🇪🇸 España** que incluye **50 emisoras españolas**: RNE Radio Nacional, RNE Radio 3, RNE Radio Clásica, Cadena SER, COPE Nacional, Onda Cero, LOS40, Rock FM España, Catalunya Ràdio, Canal Sur Radio, entre otras).
- **Emisoras personalizadas:** Añade o elimina tus propias URLs (`.mp3`, `.aac`, `.m3u`, `.pls`, `.m3u8`) con resolución de playlists y actualización dinámica de metadatos ICY (`StreamTitle`).
- **Reproducción continua sin atascos:**
  * Resincronización automática de la cola local si MPD se reinicia o vacía.
  * Reintento automático (hasta 3 intentos) ante cortes puntuales de emisoras de radio en directo.
  * Reconexión automática del canal SSE y del stream web de audio (`/api/stream` bajo HTTPS o `:4082` en HTTP) ante microcortes de Wi-Fi.

---

## 🎛️ Audio Hi-Fi y Hardware

- **Gapless Playback & Crossfade:** Transición sin pausas o fundido cruzado ajustable de 0 a 15 segundos.
- **Normalización ReplayGain:** Modo `Pista` o `Álbum` configurable en tiempo real.
- **Selector de Salidas en Caliente + Hotplug USB:** Conmuta en **Ajustes** entre:
  * Jack analógico 3.5mm integrado (`snd_bcm2835`).
  * DAC USB externo (`hw:1,0` / `plughw:CARD=...`), con botón **🔄 Redetectar DAC** y diagnóstico ALSA (`aplay -l`).
  * DAC I2S HAT (`Dual ES9023`, HiFiBerry, Allo Boss, IQaudio; configurando su `dtoverlay=` en `/boot/firmware/config.txt`).
  * Transmisión Web HTTP/HTTPS para escuchar directamente en móvil u ordenador.
- **Sleep Timer:** Temporizador de apagado automático de 15, 30, 45 o 60 minutos.

---

## 🛠️ Puesta en Marcha en Raspberry Pi y Otros Equipos

> 📖 **Guía Completa Detallada:** Consulta el manual paso a paso con diagramas de cableado de amplificador, montaje NAS, configuración de DACs I2S/USB, HTTPS y solución de problemas en [`docs/instalacion.md`](docs/instalacion.md).

### Opción 1: Instalación Automatizada en Raspberry Pi (Recomendada)
Clona el repositorio y ejecuta el instalador (instala Docker, añade grupos `docker`/`audio`, configura `.env`, reglas `udev` para USB y levanta los contenedores):
```bash
git clone https://github.com/notecortes/jukeboox3.git ~/aurabox
cd ~/aurabox
chmod +x scripts/install-rpi.sh
./scripts/install-rpi.sh
```

### Opción 2: Despliegue Manual con Docker Compose
```bash
# 1. Crear archivo privado de credenciales desde la plantilla
cp .env.example .env
# Edita .env con la IP, usuario y contraseña de tu NAS (excluido en .gitignore)
nano .env

# 2. Compilar e iniciar contenedores (en Raspberry Pi usa COMPOSE_PARALLEL_LIMIT=1)
sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build
```

### Acceso directo poniendo solo la IP (HTTPS o solo HTTP)
De serie AuraBox escucha en `:4080` (HTTP) y `:4443` (HTTPS). Para entrar escribiendo únicamente `192.168.x.x` en el móvil o navegador sin especificar puerto:
```bash
chmod +x scripts/setup-https-pi.sh

# Modo HTTPS en puerto 443 con redirección automática desde puerto 80:
./scripts/setup-https-pi.sh

# Modo solo HTTP directo en puerto 80 (http://<IP>, sin HTTPS ni avisos de certificado):
./scripts/setup-https-pi.sh --http

# Volver a los puertos originales (:4080 HTTP y :4443 HTTPS):
./scripts/setup-https-pi.sh --disable
```

### Verificación y Puertos de Acceso
```bash
./scripts/test-aurabox.sh
```
- **Interfaz Web PWA (HTTP):** `http://<IP_RASPBERRY>:4080` *(o `http://<IP_RASPBERRY>` con `--http`)*
- **Interfaz Web PWA (HTTPS):** `https://<IP_RASPBERRY>:4443` *(o `https://<IP_RASPBERRY>` con `setup-https-pi.sh`)*
- **Métricas y Salud:** `http://<IP_RASPBERRY>:4081/health` y `/metrics`
- **Audio Stream Directo:** `http://<IP_RASPBERRY>:4082` *(o `/api/stream` en el mismo origen HTTPS)*

### Auto-Montaje de Pendrives USB Manual (`udev`)
*(Este paso solo es necesario en instalaciones manuales; `./scripts/install-rpi.sh` ya lo aplica automáticamente).*
```bash
sudo cp scripts/automount-usb.sh /usr/local/bin/aurabox-usb
sudo chmod +x /usr/local/bin/aurabox-usb
```
Añade a `/etc/udev/rules.d/99-aurabox-usb.rules` y recarga con `sudo udevadm control --reload-rules`:
```udev
ACTION=="add", SUBSYSTEM=="block", KERNEL=="sd[a-z][0-9]", RUN+="/usr/local/bin/aurabox-usb add /dev/%k"
ACTION=="remove", SUBSYSTEM=="block", KERNEL=="sd[a-z][0-9]", RUN+="/usr/local/bin/aurabox-usb remove /dev/%k"
```

---

## 📡 API REST y Flujos SSE

- **Estado y Tiempo Real:** `GET /api/status`, `GET /api/events` (SSE <50ms), `GET /health` y `GET /metrics` (puerto `4081`).
- **Control del Reproductor y Audio:**
  * `POST /api/player/play`, `/pause`, `/toggle`, `/next`, `/prev`, `/seek?seconds=...`, `/repeat?enabled=...`
  * `POST /api/player/volume?value=...`, `/volume/step?delta=...`, `/volume/max?limit=...`
  * `POST /api/player/output?id=...`, `POST /api/player/outputs/refresh`, `GET /api/player/outputs/diagnostics`
  * `POST /api/player/config?replay_gain=...&crossfade=...`, `POST /api/player/sleeptimer?minutes=...`
- **Cola Jukebox:** `GET /api/queue`, `POST /api/queue/add`, `/play`, `/move`, `/remove`, `/clear`, `/shuffle`.
- **Biblioteca, Favoritas y Búsqueda:**
  * `GET /api/library/tracks`, `/recent`, `/albums`, `/album?id=...`, `/artists`, `/artist/tracks?name=...`, `/folders?path=...`
  * `GET /api/library/favorites`, `POST /api/library/favorite?id=...`, `POST /api/library/favorites/play`, `POST /api/library/scan`
  * `GET /api/search?q=...`, `GET /api/covers/...`
- **Smart Mix y Letras:** `POST /api/smartmix`, `GET /api/lyrics?title=...&artist=...`.
- **Radios Web y Streaming:** `GET / POST / DELETE /api/radios`, `POST /api/radios/play`, `GET /api/stream`.
- **Ajustes, NAS y Seguridad TLS:** `GET / POST /api/settings`, `GET / POST /api/settings/nas`, `GET /api/security/status`, `GET /api/security/ca.crt`.

