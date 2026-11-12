# Informe Exhaustivo de Auditoría de Seguridad, Robustez y Mejoras — AuraBox

**Fecha de auditoría:** 2026-11-11  
**Alcance analizado:** Backend Go (`main.go`, `internal/*`), Frontend PWA Vanilla (`web/*`), Contenedores Docker (`Dockerfile`, `docker/mpd/*`, `docker-compose.yml`, `config/mpd.conf`) y Scripts de sistema (`scripts/*`).  
**Naturaleza de la revisión:** Auditoría estática integral de código fuente, arquitectura de red, modelo de permisos en contenedor/host y estabilidad operativa 24/7 en Raspberry Pi. *(No se modificó código productivo durante la auditoría.)*

---

## 1. Resumen Ejecutivo y Veredicto General

### Veredicto

* **En una red doméstica privada de confianza:** **Aceptable tras aplicar los parches críticos de Fase A**, ya que el proyecto parte de una base técnica limpia: superficie de dependencias externa casi nula (`go.mod` con una sola librería externa), cero llamadas a `os/exec` en Go, consultas SQLite completamente parametrizadas y un motor MPD con lógica sólida de reconexión y límites de buffer en parsers binarios.
* **En redes Wi-Fi compartidas, redes de invitados o exposición directa a Internet:** **NO APTO en el estado actual.** Existen dos fallos de severidad **Crítica** (inyección de directivas en el protocolo de MPD y ausencia total de autenticación con el puerto raw `4600:6600` de MPD abierto a toda la red), además de vulnerabilidades **Altas** en el manejo de `CORS + GET mutadores`, montaje USB como `root`, generación ilimitada de certificados TLS por cabecera `Host` y permisos de contenedor.

### Tabla Resumen de Hallazgos

| ID | Severidad | Categoría | Resumen | Ubicación principal |
|---|---|---|---|---|
| **SEC-01** | **Crítica** | Seguridad / Inyección | Inyección de comandos en protocolo MPD vía `replay_gain` sin lista blanca ni sanitización de CRLF | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L562-L571), [client.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L2891-L2909) |
| **SEC-02** | **Crítica** | Autenticación / Red | Cero autenticación en HTTP/Admin + puerto raw `4600:6600` de MPD sin contraseña expuesto en `0.0.0.0` | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L256-L322), [docker-compose.yml](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L73-L75) |
| **SEC-03** | **Alta** | Navegador / CSRF / CORS | `Access-Control-Allow-Origin: *` global combinado con endpoints de control que mutan estado por `GET` | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L335-L346), [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L439-L580) |
| **SEC-04** | **Alta** | Contenedores / Privilegios | Contenedores ejecutándose como `root` sin `cap_drop`, `no-new-privileges`, ni imágenes versionadas | [Dockerfile](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/Dockerfile#L18-L45), [entrypoint.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/entrypoint.sh#L93) |
| **SEC-05** | **Alta** | Sistema Host (`udev`) | `ID_FS_LABEL` sin sanitizar en ruta de montaje ejecutada como `root` e instalador sin verificación de firma | [automount-usb.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/automount-usb.sh#L16-L48), [install-rpi.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/install-rpi.sh#L29-L31) |
| **SEC-06** | **Alta** | TLS / DoS | `EnsureHost(r.Host)` sin lista blanca re-emite claves ECDSA y escribe a la microSD ante cualquier `Host` falso | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1495-L1537), [certs.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/tlsutil/certs.go#L111-L140) |
| **SEC-07** | **Media-Alta** | Red / SSRF | `ResolveStreamURLs` acepta direcciones internas/loopback/link-local y `LogoURL` carece de validación de esquema | [radio.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/radio.go#L20-L93), [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1196-L1285) |
| **SEC-08** | **Media** | Frontend / XSS / CSP | Brechas de `innerHTML` sin escapar, inyección CSS `url()`, `onerror=` inline y ausencia de `CSP` / `HSTS` | [views.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L1156), [player.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/player.js#L342), [lyrics.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/lyrics.js#L85) |
| **SEC-09** | **Media** | Sistema de Archivos | `isSafeMusicPath` usa `HasPrefix` sin barra final ni `EvalSymlinks`, permitiendo fuga por enlaces simbólicos | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L413-L424), [scanner.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/scanner.go#L745-L785) |
| **SEC-10** | **Media** | Disponibilidad / Memoria | Cuerpos JSON y respuestas externas sin `LimitReader`; mapas en memoria `coverCache` y `lyrics.cache` sin cota | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L603), [lrclib.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/lyrics/lrclib.go#L47-L285) |
| **SEC-11** | **Media** | Secretos / Configuración | `POST /api/settings` sin lista blanca; `nas_pass` en texto plano sin efecto real en CIFS; `.env` sin `0600` | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1312-L1448), [setup-https-pi.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/setup-https-pi.sh#L217-L279) |
| **SEC-12** | **Baja / Info** | Fuga de Info / Red | Errores internos literales por HTTP, `api_key` fija de Last.fm y pérdida de IP real detrás de `systemd-socket-proxyd` | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L537), [smartmix.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/smartmix/smartmix.go#L181) |
| **ROB-01** | **Alta** | Robustez HTTP | Middleware gzip escribe sobre respuestas `304`/`204`/`HEAD`/`Range`, recomprime JPEG/PNG y omite `Unwrap()` | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L355-L402) |
| **ROB-02** | **Alta** | Robustez Audio | `SelectAudioOutput` desactiva las demás salidas antes de confirmar que `enableoutput` funcione, sin rollback | [client.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L2755-L2769) |
| **ROB-03** | **Alta** | Despliegue / Sincronía | `config/mpd.conf` ignorado en runtime, ruta USB host/contenedor desalineada y nombres ALSA sin sanitizar en conf | [docker-compose.yml](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L49-L82), [entrypoint.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/entrypoint.sh#L227) |
| **ROB-04** | **Media** | Biblioteca / UX | Escáner no purga pistas borradas/USB desconectado; botón "Subir un nivel" en carpetas produce `403 Forbidden` | [scanner.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/scanner.go#L114-L145), [views.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L648-L701) |
| **ROB-05** | **Media** | Metadatos / CPU | `metadata.go` omite `.m4a`/`.ogg`/`.opus`, `uint32` VBR arriesga desborde y `lrclib.go` compila regex en cada llamada | [metadata.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/metadata.go#L40-L522), [lrclib.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/lyrics/lrclib.go#L72-L310) |
| **ROB-06** | **Media** | Ciclo de Vida / SSE | Cierre abrupto `Close()` sin `Shutdown(ctx)` y clientes SSE sin límite numérico ni deadline por escritura | [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L248-L252), [sse.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/events/sse.go#L44-L169) |
| **ROB-07** | **Baja** | SQLite / Calidad | `LIKE` sin escapar `%` ni `_` y falta de pruebas unitarias de validación/seguridad en `main_test.go` | [sqlite.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go#L792-L1346), [main_test.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main_test.go) |

---

## 2. Superficie de Ataque y Modelo de Amenazas

### 2.1. Inventario de Puertos y Servicios Expuestos

| Puerto | Origen | Bind Actual | Servicio | ¿Necesita exposición LAN? |
|---|---|---|---|---|
| **4080/TCP** | `aurabox-core` | `0.0.0.0:4080` | Web PWA + API REST + Redirect TLS | Sí (o solo `127.0.0.1` si puerto 80/443 está activo) |
| **4443/TCP** | `aurabox-core` | `0.0.0.0:4443` | Web PWA + API REST sobre TLS 1.2+ | Sí (o solo `127.0.0.1` si puerto 443 está activo) |
| **4081/TCP** | `aurabox-core` | `0.0.0.0:4081` | `/health` y `/metrics` | **No** (basta `127.0.0.1:4081` para checks locales) |
| **4082/TCP** | `aurabox-mpd` | `0.0.0.0:4082` | Stream HTTP `lame` 320kbps | **No** (el cliente web usa `/api/stream` en `:4080/:4443`) |
| **4600/TCP** | `aurabox-mpd` | `0.0.0.0:4600->6600` | Consola TCP cruda de MPD | **No** (`aurabox-core` conecta por socket Unix `/run/mpd/socket` o red interna `aurabox-net`) |
| **80 / 443** | `systemd` (Pi) | `0.0.0.0:80,443` | `systemd-socket-proxyd` -> `127.0.0.1:4080/4443` | Opcional para URL sin puerto (`./scripts/setup-https-pi.sh`) |

### 2.2. Actores de Amenaza Considerados

1. **Cliente no confiable o invitado en la misma LAN Wi-Fi:** Cualquier móvil, televisor IoT o portátil en la subred local puede alcanzar todos los puertos anteriores sin credenciales.
2. **Navegador del propietario visitando una web externa maliciosa (CSRF/CORS):** Mientras el usuario escucha música en AuraBox, una pestaña externa abierta en el mismo navegador puede intentar invocar la API local del reproductor.
3. **Medios físicos o fuentes externas malformadas:** Un pendrive USB con etiqueta o enlaces simbólicos manipulados, un archivo MP3/FLAC corrupto, o una URL/playlist `.pls`/`.m3u` o respuesta externa de LRCLIB/Last.fm manipulada.
4. **Degradación operativa 24/7 en Raspberry Pi:** Escrituras innecesarias a la tarjeta microSD, pérdida de salidas de audio al fallar el DAC, fugas de goroutines SSE o presión de RAM dentro del límite de `256M`.

---

## 3. Controles Positivos y Fortalezas Verificadas

La base de código incorpora decisiones de ingeniería muy acertadas que conviene conservar intactas:

1. **Superficie mínima de dependencias y ejecución de procesos:**
   * En backend (`go.mod`), la única dependencia externa es `github.com/mattn/go-sqlite3 v1.14.53`; el resto descansa sobre la biblioteca estándar de Go `1.22.12`.
   * Se verificó en todo el árbol `.go` que **no existen llamadas a `os/exec`**, eliminando de raíz la inyección de shell desde la API web.
   * El frontend en `web/` utiliza exclusivamente **HTML5, CSS3 y ES Modules vanilla** sin dependencias Node.js ni uso de `eval()`, `new Function()` o `document.write()`.
2. **Prevención de Inyección SQL y concurrencia WAL robusta:**
   * Todas las consultas en [internal/db/sqlite.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go) emplean marcadores `?` parametrizados (incluso [`GetTracksByIDs`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go#L853-L911) genera únicamente la lista de `?` parametrizados por lotes de 500).
   * [`InitDB`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go#L42-L110) separa explícitamente una conexión única serializada de escritura (`SetMaxOpenConns(1)` + `writeMu`) de un pool concurrente de lectura (`readConn`, `SetMaxOpenConns(4)`) en modo `WAL`, `busy_timeout=10000`, `mmap_size=33554432` y `wal_autocheckpoint=500`.
   * [`DeleteRadio`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go#L1278-L1297) impide borrar emisoras integradas mediante `WHERE id = ? AND is_default = 0`.
3. **Acotación de rutas en MPD y salvaguarda de volumen:**
   * [`mpdQuote`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L479-L485) elimina `\r` y `\n` y escapa `\` y `"` en comandos `add`/`addid`.
   * [`SetMaxVolumeLimit`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L1761-L1795) acota el tope entre `30` y `100` y recorta el volumen activo si lo excede.
4. **Diseño sólido de PKI local y redirección TLS:**
   * [internal/tlsutil/certs.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/tlsutil/certs.go) genera claves ECDSA P-256 (`:212, :319`), exige `MinVersion: tls.VersionTLS12` (`:105`), limita la validez del certificado hoja a 800 días (`:350`, por debajo del tope de 825 días de Apple) y escribe las claves `.key` con permiso `0600` (`:260, :375`).
   * [`buildHTTPSURLForHost`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1566-L1591) valida la ausencia de caracteres ` \t\r\n/@\` y fuerza prefijo `/` en `reqURI`, evitando redirecciones abiertas o inyección CRLF.
5. **Lectura acotada en parsers de audio y listas de radio:**
   * [internal/scanner/metadata.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/metadata.go) limita `tagSize <= 20MB` en ID3v2 (`:334`), salta frames binarios `APIC`/`PRIV`/`GEOB` por `Seek` cuando superan 64 KB (`streamID3v2Frames:282-318`), acota frames de texto a 8192 bytes (`:305, :379`), bloques Vorbis a 2 MB (`:103`), `numComments <= 4096` (`:149`) y `commentLen <= 4096` (`:158`).
   * [internal/mpd/radio.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/radio.go) limita listas `.pls`/`.m3u` a `64 KB` (`maxPlaylistBodyBytes:13, :76`) con timeout de 4 segundos (`:16`).
6. **Aislamiento de recursos, SSE no bloqueante y Service Worker estable:**
   * [docker-compose.yml](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml) impone `memory: 256M` en ambos servicios (`GOMEMLIMIT=200MiB` en Go) y monta `./music/local`, `nas_music` y `./music/usb` en modo solo lectura (`:ro`).
   * [internal/events/sse.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/events/sse.go) usa envío no bloqueante (`:74-78`) para no congelar `Broadcast` ante clientes lentos.
   * [web/sw.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/sw.js) excluye `/api/`, cabeceras `Range` y flujos `audio`/`video` (`:24-44`) y entrega respuestas `503` bien formadas en fallo de red (`:47-149`), evitando fallos de `FetchEvent.respondWith()`.

---

## 4. Hallazgos de Seguridad Detallados (`SEC-01` a `SEC-12`)

### SEC-01 [CRÍTICA] Inyección de Comandos en el Protocolo MPD vía `replay_gain`

* **Ubicación:**
  * [`handleAudioConfig`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L562-L571)
  * [`SetAudioConfig`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L2891-L2909), [`tryConnect`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L356), [`RefreshAudioOutputs`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L2870) y [`NewClient`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L137-L139)
* **Análisis técnico:**  
  [`handleAudioConfig`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L562-L571) lee el parámetro `replay_gain` de la query HTTP sin validar contra una lista blanca y lo pasa directamente a `SetAudioConfig`, que formatea una línea textual sobre el socket de MPD: `fmt.Sprintf("replay_gain_mode %s\n", replayGain)` (`client.go:2898`). A diferencia de `mpdQuote`, aquí no se eliminan `\r` ni `\n`. Además, el valor se persiste en SQLite (`app_settings.replay_gain`), por lo que vuelve a reenviarse automáticamente en cada reconexión (`tryConnect:356`), arranque (`NewClient:137-139`) y recarga de salidas (`:2870`).
* **Impacto:**  
  Cualquier parámetro `replay_gain` que incluya un salto de línea permite inyectar comandos arbitrarios al demonio MPD (alterar salidas, borrar/modificar cola, fijar volumen hardware al 100% saltando `max_volume_limit` o cargar URIs externas) con persistencia tras reinicios.
* **Remediación recomendada:**
  Validar estrictamente `replay_gain` mediante lista blanca tanto en la capa HTTP como dentro del cliente MPD (y validar el rango de `crossfade`, p. ej. `0..15` segundos).

```go
// normalizeReplayGainMode valida y normaliza el modo ReplayGain contra los valores oficiales de MPD.
func normalizeReplayGainMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case "", "auto":
		return "auto", nil
	case "off", "track", "album":
		return mode, nil
	default:
		return "", fmt.Errorf("replay_gain inválido: solo se permite off, track, album o auto")
	}
}
```

---

### SEC-02 [CRÍTICA] Ausencia Total de Autenticación y Puerto Crudo de MPD (`4600:6600`) Expuesto en LAN

* **Ubicación:**
  * [`registerRoutes`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L256-L322)
  * [docker-compose.yml](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L15-L18) y [puerto 4600](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L73-L75)
  * [docker/mpd/entrypoint.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/entrypoint.sh#L94-L96) y [config/mpd.conf](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/config/mpd.conf#L14-L16)
* **Análisis técnico:**
  1. Ninguna ruta de AuraBox (`4080`, `4443`, `4081`) exige sesión, token ni clave para operaciones administrativas (`/api/settings`, `/api/settings/nas`, `/api/player/volume/max`, `/api/player/output`, `/api/library/scan`).
  2. El contenedor `aurabox-mpd` expone directamente el puerto TCP `4600:6600` sobre `0.0.0.0` sin directiva `password` en MPD, a pesar de que `aurabox-core` ya se comunica por el socket compartido `mpd_run` (`/run/mpd/socket`) o dentro de la red puente interna `aurabox-net`.
* **Impacto:**
  Cualquier equipo o dispositivo comprometido en la red local puede conectarse directo al puerto `4600` y elevar el volumen al 100% (ignorando la salvaguarda auditiva `max_volume_limit` de AuraBox, con riesgo de daño en altavoces o audición si el amplificador está alto), expulsar salidas ALSA o vaciar la base de MPD. De forma análoga, `:4080` y `:4081` exponen ajustes del sistema y metadatos de NAS sin barrera de acceso.
* **Remediación recomendada:**
  1. Eliminar la publicación LAN de los puertos internos en [docker-compose.yml](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml) (o ligarlos a `127.0.0.1` si se necesitan localmente para diagnóstico):
     * Quitar `4600:6600` y `4082:4082` de `ports:` en `mpd` (el backend ya consume ambos dentro de `mpd_run` y `aurabox-net`). Si un cliente MPD externo es imprescindible, publicarlo solo como `"127.0.0.1:4600:6600"` y configurar contraseña MPD.
     * Ligar `"127.0.0.1:4081:4081"` para `/health` y `/metrics`.
  2. Si hay acceso de red compartida, añadir autenticación opcional basada en `AURABOX_ADMIN_PIN` / token de sesión con cookie `HttpOnly; Secure; SameSite=Strict` para endpoints de configuración sensible (ver Fase D).

---

### SEC-03 [ALTA] CORS Global (`*`) Combinado con Endpoints Mutadores por `GET` (CSRF)

* **Ubicación:**
  * [`corsMiddleware`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L335-L346), [`handleAudioStream`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1389) y [`ServeHTTP` SSE](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/events/sse.go#L113)
  * Handlers sin comprobación de `r.Method`: [`handlePlay`..`handleSleepTimer`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L439-L579), [`handleQueuePlay`..`handleQueueShuffle`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L713-L760), [`handleLibraryScan` y `handleFavoriteToggle`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L948-L975)
* **Análisis técnico:**
  Aunque el frontend ([web/js/api.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/api.js#L34-L225)) envía correctamente `method: 'POST'` para todas las mutaciones de reproducción, cola y biblioteca, el backend **no comprueba `r.Method`** en esas rutas (a diferencia de `handleRadios` o `handleRadioPlay`). Sumado a `Access-Control-Allow-Origin: *`, cualquier sitio web externo abierto en el navegador de un móvil dentro de la LAN puede realizar peticiones simples (`GET` o `POST text/plain`) o leer respuestas JSON de la biblioteca/configuración NAS.
* **Impacto:**
  1. **CSRF sin JavaScript complejo:** Una etiqueta o navegación desde otro origen puede cambiar `max_volume_limit`, subir el volumen, vaciar la cola o disparar **SEC-01** usando peticiones `GET` simples con parámetros en query string.
  2. **Lectura cross-origin de datos locales:** Cualquier página visitada por el usuario puede consultar `/api/settings/nas` (obteniendo alias, IP interna, recurso compartido y usuario del NAS) o `/api/library/tracks`.
* **Remediación recomendada:**
  1. Exigir explícitamente `POST` en todos los handlers que alteran estado (`requireMethod(http.MethodPost, ...)`), moviendo además parámetros sensibles del query string al cuerpo JSON cuando aplique.
  2. Restringir `corsMiddleware` eliminando `*`: para una PWA servida en el mismo origen (`https://IP` / `http://IP:4080`), el navegador **no necesita CORS**; si se desea compatibilidad con hosts locales concretos, validar la cabecera `Origin` contra la lista de orígenes permitidos.

```go
// requireMethod envuelve un handler HTTP rechazando métodos distintos al permitido con 405 Method Not Allowed.
func requireMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// strictSameOriginCORSMiddleware solo permite peticiones del mismo origen o sin cabecera Origin (clientes nativos/curl).
func strictSameOriginCORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			parsedOrigin, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(parsedOrigin.Host, r.Host) {
				http.Error(w, "origen CORS no autorizado", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

---

### SEC-04 [ALTA] Contenedores Ejecutándose como `root` y Carencia de Endurecimiento Docker

* **Ubicación:**
  * [Dockerfile](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/Dockerfile#L1-L45)
  * [docker/mpd/Dockerfile](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/Dockerfile#L1-L22), [entrypoint.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/entrypoint.sh#L93) y [mpd.conf](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/config/mpd.conf#L13)
  * [docker-compose.yml](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L19-L22) y [mpd volumes](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L76-L89)
* **Análisis técnico:**
  1. El contenedor `aurabox-core` no define directiva `USER`, por lo que corre como UID `0` (`root`).
  2. El contenedor `aurabox-mpd` configura explícitamente `user "root"` en `mpd.runtime.conf` (`entrypoint.sh:93`) y `config/mpd.conf:13`, y monta la cookie de PulseAudio en `/root/.config/pulse/cookie`.
  3. Ninguno de los dos servicios incluye `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]` ni sistema de archivos raíz en solo lectura (`read_only: true` con `tmpfs`).
  4. Ambos `Dockerfile` usan etiquetas flotantes (`golang:1.22-alpine`, `alpine:latest`), dificultando builds reproducibles y auditorías de CVE en el tiempo.
* **Impacto:**
  Ante cualquier fallo en un decodificador multimedia de `mpd` (p. ej. FFmpeg/FLAC/Vorbis procesando un stream o fichero malformado) o en el servidor web Go, un atacante obtendría permisos `root` dentro del contenedor (con acceso directo al bus `/dev/snd` y volúmenes persistentes como `./data` con claves CA `.key`).
* **Remediación recomendada:**
  * Fijar versión concreta de Alpine (ej. `alpine:3.20` y `golang:1.22.12-alpine3.20`).
  * Crear usuario dedicado no privilegiado en `aurabox-core` (`aurabox`, UID `10001`), darle propiedad únicamente de `/app/data`, usar `cap_drop: ["ALL"]` y `no-new-privileges:true`.
  * En `aurabox-mpd`, usar `user "mpd"` (añadido al grupo cuyo GID coincide con `/dev/snd` del host, típicamente `29`), manteniendo la lista mínima de capacidades (en realidad `aurabox-mpd` con usuario `mpd` y acceso por grupo `audio` tampoco necesita capacidades de root una vez iniciados `/run/mpd` y `/var/lib/mpd`).

---

### SEC-05 [ALTA] Path Traversal por Etiqueta USB en Script `udev` (`root`) e Instalador Remoto

* **Ubicación:**
  * [scripts/automount-usb.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/automount-usb.sh#L16-L48)
  * [scripts/install-rpi.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/install-rpi.sh#L29-L31) y [regla udev](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/install-rpi.sh#L68-L77)
* **Análisis técnico:**
  1. El script `/usr/local/bin/aurabox-usb` se ejecuta como **`root`** desde `udevd` al conectar un pendrive (`install-rpi.sh:73-74`). Toma `ID_FS_LABEL` directamente de `blkid` y concatena `TARGET_DIR="${MOUNT_BASE}/${ID_FS_LABEL}"` (`automount-usb.sh:21`) sin sanitizar secuencias `..`, barras `/`, espacios ni caracteres especiales, antes de invocar `mkdir -p`, `mount`, `umount -l` y `rmdir`.
  2. Tampoco utiliza opciones de montaje restrictivas (`nosuid,nodev,noexec`), exponiendo el host si el sistema de archivos montado es Linux (`ext4`) con binarios `setuid`.
  3. Además, `automount-usb.sh` dispara `curl -s -X POST http://localhost:4080/api/library/scan` (`:35, :47`), lo cual falla silenciosamente al activar `FORCE_HTTPS=true` porque `curl` sin `-L` se detiene en el `307 Temporary Redirect`.
  4. En [scripts/install-rpi.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/install-rpi.sh#L29-L31), el instalador escribe en una ruta predecible `/tmp/get-docker.sh` (riesgo de ataque por symlink en `/tmp` compartido) y ejecuta código remoto con `sudo sh` sin comprobación criptográfica o paquete APT oficial.
* **Impacto:**
  Un pendrive cuya etiqueta de volumen contenga segmentos `../` puede provocar montajes o desmontajes (`umount -l`) sobre directorios del sistema host fuera de `/music/usb` con privilegios de `root`.
* **Remediación recomendada:**
  Sanitizar estrictamente `ID_FS_LABEL` a `[A-Za-z0-9_-]`, usar `mktemp -d` para descargas temporales o instalar `docker.io`/`docker-compose` desde repositorios firmados de Raspberry Pi OS (`apt`), montar siempre con `ro,nosuid,nodev,noexec` y comprobar que `realpath` del destino cuelga de `MOUNT_BASE`.

```bash
# sanitize_usb_label normaliza una etiqueta de volumen a caracteres seguros evitando path traversal.
sanitize_usb_label() {
    local raw_label="$1"
    local clean
    clean=$(printf '%s' "$raw_label" | tr -cs 'A-Za-z0-9._-' '_' | sed 's/^[._-]*//; s/[._-]*$//' | cut -c1-48)
    if [ -z "$clean" ] || [ "$clean" = "." ] || [ "$clean" = ".." ]; then
        clean="usb_drive"
    fi
    printf '%s' "$clean"
}
```

---

### SEC-06 [ALTA] Cabecera `Host` sin Lista Blanca Permite DoS de CPU/microSD en Certificados y Reflejo de `https_url`

* **Ubicación:**
  * [`httpsRedirectMiddleware`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1493-L1498), [`handleSecurityStatus`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1522-L1537)
  * [`EnsureHost`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/tlsutil/certs.go#L111-L140), [`randomSerialNumber`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/tlsutil/certs.go#L426-L429)
* **Análisis técnico:**
  1. En **cada** petición HTTP entrante (`main.go:1495`) o consulta a `/api/security/status` (`main.go:1523`), el servidor llama a `s.certManager.EnsureHost(r.Host)`. Si el nombre o IP de `r.Host` no estaba en el certificado, `EnsureHost` bloquea `cm.mu.Lock()`, añade el valor al mapa de SANs de por vida, genera una nueva clave ECDSA P-256 (`ecdsa.GenerateKey`), firma un certificado X.509 (`issueLeafLocked`) y escribe `aurabox.crt` + `aurabox.key` en la tarjeta microSD (`certs.go:374-375`).
  2. Como `r.Host` procede directamente del cliente sin comprobar que corresponda a una IP local real o a `TLS_EXTRA_HOSTS`, una ráfaga con cabeceras `Host` aleatorias dispara miles de generaciones ECDSA y escrituras síncronas en flash, degradando la CPU de la Pi y desgastando la microSD, además de inflar indefinidamente los SAN del certificado.
  3. Adicionalmente, `randomSerialNumber` (`certs.go:426-429`) toma `[0, 2^128)`, por lo que existe probabilidad teórica de generar serial `0` (RFC 5280 exige enteros estrictamente positivos `>= 1`).
* **Impacto:**
  Denegación de servicio por consumo de CPU, bloqueo mutuo sobre `cm.mu` al conectar clientes TLS legítimos, crecimiento ilimitado de SANs y desgaste prematuro de la tarjeta microSD en Raspberry Pi; además, `/api/security/status` refleja cualquier `Host` externo no confiable en `https_url`.
* **Remediación recomendada:**
  Restringir `EnsureHost` a IPs privadas/locales válidas pertenecientes realmente al host (o definidas en `TLS_EXTRA_HOSTS`, `aurabox.local`, `<hostname>.local`) e imponer un límite máximo estricto (p. ej. 32 SANs) con enfriamiento mínimo entre re-emisiones; en `randomSerialNumber`, garantizar `serial >= 1` (`new(big.Int).Add(val, big.NewInt(1))`).

---

### SEC-07 [MEDIA-ALTA] SSRF en Resolución de Listas de Radio y Ausencia de Validación en `LogoURL`

* **Ubicación:**
  * [`ValidateHTTPStreamURL`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/radio.go#L20-L37) y [`ResolveStreamURLs`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/radio.go#L40-L93)
  * [`handleRadios`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1196) y [`handleRadioPlay`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1282)
* **Análisis técnico:**
  1. `ValidateHTTPStreamURL` únicamente comprueba que el esquema sea `http` o `https` y que no haya `\r\n`. No filtra direcciones de loopback (`127.0.0.1`, `::1`), rangos privados RFC1918 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), link-local/metadatos (`169.254.0.0/16`), ni nombres internos Docker (`mpd`, `aurabox-core`, `localhost`).
  2. `radioPlaylistHTTPClient` (`radio.go:15`) sigue redirecciones HTTP por defecto sin revalidar la IP resuelta tras DNS ni el destino del `Location`.
  3. Una URL no terminada en `.pls`/`.m3u` se pasa directamente a MPD (`add "<url>"`), permitiendo que el propio MPD sondee puertos internos.
  4. El campo opcional `LogoURL` aceptado en `POST /api/radios` y `POST /api/radios/play` no valida esquema (`http://`, `https://` o ruta local `/assets/...`).
* **Impacto:**
  **Server-Side Request Forgery (SSRF)** desde `aurabox-core` o `aurabox-mpd` hacia servicios internos (`http://127.0.0.1:4081/...`, otros contenedores Docker, router doméstico o servicios en la nube/metadatos si se despliega en VPS) e inyección de esquemas no seguros en `LogoURL` (ver **SEC-08**).
* **Remediación recomendada:**
  Usar un `http.Transport` con `Control` personalizado que resuelva e inspeccione la IP destino en el momento exacto de establecer el socket TCP (`net.Dialer.Control`), bloqueando loopback, privadas, link-local y multicast tanto en peticiones directas como en redirecciones (y validar las URLs finales antes de enviarlas a MPD, salvo que un ajuste explícito `ALLOW_LAN_RADIOS=true` habilite redes locales concretas). Validar igualmente `LogoURL`.

```go
// isPublicRoutableIP verifica que una IP no pertenezca a loopback, redes privadas, CGNAT ni link-local.
func isPublicRoutableIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}

// newSafeExternalHTTPClient crea un cliente HTTP resistente a SSRF y DNS rebinding validando cada IP al conectar.
func newSafeExternalHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if !isPublicRoutableIP(ip) {
				return fmt.Errorf("conexión bloqueada hacia dirección no pública: %s", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
	}
}
```

---

### SEC-08 [MEDIA] Brechas de XSS en el DOM, Inyección CSS `url()`, `onerror=` Inline y Ausencia de CSP/HSTS

* **Ubicación:**
  * [`triggerSmartMix`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L1156): `${t.cover_url || '/assets/default-cover.svg'}` sin `escapeHtml()`.
  * [`renderFolderBrowser`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L784): `${ext}` insertado sin `escapeHtml()`.
  * [`renderNowPlaying`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/player.js#L342): ``bg.style.backgroundImage = `url('${coverSrc}')` `` sin validar esquema ni escapar comillas CSS/paréntesis.
  * [`loadLyricsForTrack`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/lyrics.js#L85): `${res?.plain || ...}` dentro de `innerHTML`.
  * Manejadores inline `onerror="this.onerror=null;this.src='/assets/default-cover.svg';"` en [views.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L285), [311](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L311), [437](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L437), [977](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L977) y [queue.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/queue.js#L122), además de `onclick=` en [sw.js](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/sw.js#L83).
  * Cabeceras HTTP en [`compressionAndSecurityMiddleware`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L374-L379): falta `Content-Security-Policy` y `Strict-Transport-Security`, y mantiene la obsoleta `X-XSS-Protection: 1; mode=block`.
* **Análisis técnico:**
  Aunque gran parte de `views.js` y `queue.js` utiliza `escapeHtml()` (duplicado en ambos archivos), cualquier valor de `LogoURL` enviado en `/api/radios/play` termina en `track.cover_url` y se inserta sin filtro en `player.js:342` (contexto CSS `url('...')`) y en `views.js:1156` (atributo HTML `src="..."`). Por otro lado, `escapeHtml` solo neutraliza `< > " ' &` y **no bloquea URLs con esquema `javascript:` o `data:text/html`**. Finalmente, los `onerror=` inline impiden activar una `Content-Security-Policy` estricta (`script-src 'self'`).
* **Impacto:**
  Inyección de estilos CSS, carga de recursos arbitrarios desde `cover_url`/`logo_url` o ejecución de HTML/JS en el contexto del origen ante datos no sanitizados, sin la segunda línea de defensa que aporta una CSP estricta.
* **Remediación recomendada (Vanilla JS + Go):**
  1. Centralizar `escapeHtml` y añadir `sanitizeCoverURL` en un módulo común `web/js/sanitize.js`.
  2. Sustituir `onerror="..."` por un manejador delegado en `document` (`addEventListener('error', ..., true)` en captura, ya que el evento `error` de `<img>` no burbujea) y reemplazar `innerHTML` en `lyrics.js:85` por `textContent`.
  3. Añadir cabecera `Content-Security-Policy` y, sobre HTTPS (`r.TLS != nil`), valorar `Strict-Transport-Security` cuando el usuario tenga CA instalada.

```javascript
// sanitizeCoverURL garantiza que la URL de carátula use solo rutas locales /assets/, /api/covers/ o http(s).
export function sanitizeCoverURL(rawUrl) {
  const fallback = '/assets/default-cover.svg';
  if (!rawUrl || typeof rawUrl !== 'string') return fallback;
  const trimmed = rawUrl.trim();
  if (trimmed.startsWith('/assets/') || trimmed.startsWith('/api/covers/')) {
    return trimmed;
  }
  try {
    const parsed = new URL(trimmed, window.location.origin);
    if (parsed.protocol === 'http:' || parsed.protocol === 'https:') {
      return parsed.href;
    }
  } catch (_) {}
  return fallback;
}
```

```javascript
// installImageFallbackListener sustituye atributos inline onerror para permitir CSP estricta sin unsafe-inline.
export function installImageFallbackListener() {
  document.addEventListener('error', (event) => {
    const target = event.target;
    if (target && target.tagName === 'IMG' && !target.dataset.fallbackApplied) {
      target.dataset.fallbackApplied = '1';
      target.src = '/assets/default-cover.svg';
    }
  }, true);
}
```

---

### SEC-09 [MEDIA] Contención Débil de Rutas (`isSafeMusicPath`), Fuga por Symlinks y Copia Sin Límites de Carátulas

* **Ubicación:**
  * [`isSafeMusicPath`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L413-L424)
  * [`handleQueueAdd`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L633-L683), [`handleLibraryFolders`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L855-L910), [`handleLyrics`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1105)
  * [`checkLocalLRC`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/lyrics/lrclib.go#L94-L108)
  * [`findOrCacheCover`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/scanner.go#L745-L760) y [`copyFile`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/scanner.go#L770-L785)
* **Análisis técnico:**
  1. **Prefijo sin límite de directorio:** `strings.HasPrefix(clean, s.localDir)` considera válido `/music/local_otro` si `s.localDir` es `/music/local`. Debe usarse `filepath.Rel(root, target)` comprobando que no comience por `..`.
  2. **Rutas con esquema HTTP en API de archivos:** `isSafeMusicPath` devuelve `true` para `http://` y `https://` (`main.go:417-418`), pero en `handleQueueAdd` (`main.go:649-667`) e `handleLyrics` (`lrclib.go:100`), esa cadena entra a funciones de sistema de archivos (`os.Stat`, `ScanFolderTracks`, `os.ReadFile`).
  3. **`handleLibraryFolders` mezcla ruta sucia y limpia:** Valida `cleanSubpath := filepath.Clean(subpath)` (`main.go:855`), lee `cleanSubpath` (`:861`), pero vuelve a construir rutas con la variable original `subpath` sin limpiar (`:881, :910`).
  4. **Ausencia de `filepath.EvalSymlinks` y copia sin cotas en `copyFile`:** Si un pendrive USB o carpeta NAS contiene un enlace simbólico llamado `cover.jpg` apuntando a `/app/data/aurabox.db` o a un dispositivo especial, `findOrCacheCover` (`scanner.go:754`) invoca `copyFile` sin verificar `os.Lstat`, sin comprobar que sea archivo regular ni cabecera mágica de imagen, ni limitar su tamaño con `io.LimitReader`, exponiéndolo estáticamente bajo `/api/covers/<md5>.jpg`.
* **Impacto:**
  Exfiltración inadvertida de archivos del contenedor mediante enlaces simbólicos en USB/NAS hacia `/api/covers/`, o agotamiento de almacenamiento en `./data/covers` por archivos gigantes nombrados `cover.jpg`.
* **Remediación recomendada:**
  Implementar verificación estricta de contención con `filepath.EvalSymlinks` + `filepath.Rel`, separar la validación de URLs de radio respecto a rutas locales y limitar `copyFile` únicamente a ficheros regulares (`Mode().IsRegular()`) de hasta p. ej. 10 MB verificando cabecera JPEG/PNG/WebP.

```go
// isStrictlyInsideBase comprueba mediante filepath.Rel y EvalSymlinks que target reside dentro de alguna raíz permitida.
func isStrictlyInsideBase(target string, allowedRoots ...string) bool {
	if target == "" {
		return false
	}
	cleanTarget := filepath.Clean(target)
	realTarget, err := filepath.EvalSymlinks(cleanTarget)
	if err != nil {
		return false
	}
	for _, root := range allowedRoots {
		if root == "" {
			continue
		}
		realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(realRoot, realTarget)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
```

---

### SEC-10 [MEDIA] Entradas JSON, Respuestas Externas y Cachés en Memoria Sin Cota (`256M` Límite de RAM)

* **Ubicación:**
  * Decodificadores `json.NewDecoder(r.Body)` sin `http.MaxBytesReader` en [`main.go`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L603): líneas `603`, `1000`, `1067`, `1172`, `1249`, `1314`, `1426`.
  * Respuestas externas de `LRCLIB`, `Last.fm` y `MusicBrainz` sin `io.LimitReader`: [lrclib.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/lyrics/lrclib.go#L259-L283) y [smartmix.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/smartmix/smartmix.go#L199-L232).
  * Mapas en memoria sin límite ni desalojo LRU: `lyrics.Service.cache` ([lrclib.go:47](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/lyrics/lrclib.go#L47)) y `LibraryScanner.coverCache` ([scanner.go:44](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/scanner.go#L44)).
* **Análisis técnico:**
  El contenedor Go tiene un límite estricto de `256M` (`GOMEMLIMIT=200MiB`), ideal para Raspberry Pi. Sin embargo, aceptar cuerpos JSON de tamaño arbitrario en endpoints `POST` o decodificar JSON sin tope desde APIs externas de terceros facilita picos de memoria. Además, las cachés `cache` y `coverCache` crecen monótonamente durante la vida del proceso en ejecuciones 24/7 con bibliotecas grandes.
* **Impacto:**
  Saturación del recolector de basura o reinicio por **OOMKill (exit 137)** en Raspberry Pi con colecciones masivas o tráfico malintencionado.
* **Remediación recomendada:**
  * Envolver todo `r.Body` JSON en `r.Body = http.MaxBytesReader(w, r.Body, 1<<20)` (1 MB).
  * Limitar las lecturas JSON externas a `io.LimitReader(resp.Body, 2<<20)` (2 MB).
  * Aplicar un límite máximo de entradas en `lyrics.Service.cache` (ej. 1000) y `coverCache` (ej. 5000) con limpieza FIFO/LRU simple.

---

### SEC-11 [MEDIA] Sobrescritura Arbitraria de `app_settings`, `nas_pass` en Texto Plano Desincronizado y Permisos `.env`

* **Ubicación:**
  * [`handleSettings`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1311-L1332)
  * [`handleNASConfig`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L1416-L1489) frente a [`nas_music` CIFS](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L95-L100)
  * [scripts/install-rpi.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/install-rpi.sh#L45) y [scripts/setup-https-pi.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/setup-https-pi.sh#L217-L279)
* **Análisis técnico:**
  1. `POST /api/settings` itera todo el mapa JSON enviado y ejecuta `SetSetting(k, v)` sin lista blanca (`main.go:1318-1320`). Cualquier cliente puede sobrescribir claves internas críticas (`replay_gain`, `max_volume_limit`, `scanner_version`, `nas_pass`, `active_audio_output_name`).
  2. `POST /api/settings/nas` guarda `nas_pass` en texto plano dentro de `aurabox.db`, aunque en Docker el volumen `nas_music` (`docker-compose.yml:95-100`) se monta únicamente en tiempo de creación del volumen leyendo `.env` (`NAS_HOST`, `NAS_USER`, `NAS_PASS`). Guardar la contraseña en SQLite desde la interfaz web **no remonta el NAS** y deja un secreto innecesario en disco.
  3. Los scripts de instalación/HTTPS crean `.env` con `cp .env.example .env` sin restringir permisos (`chmod 600 .env`), dejándolo legible por otros usuarios locales del sistema (`0644`).
* **Impacto:**
  Manipulación de parámetros internos vía `/api/settings`, confusión operativa al editar credenciales del NAS en la web y exposición innecesaria de `nas_pass` en `aurabox.db` / `.env`.
* **Remediación recomendada:**
  * Restringir `POST /api/settings` a una lista blanca cerrada de preferencias seguras de interfaz/audio validadas (`crossfade`, `replay_gain`, `max_volume_limit`).
  * No almacenar `nas_pass` en SQLite si el montaje CIFS depende de Docker/`.env`; mostrar en la UI el estado de montaje de `/music/nas` e instrucciones claras si se requiere cambio de credenciales.
  * Aplicar `chmod 600 .env` al crear o actualizar `.env` en ambos scripts.

---

### SEC-12 [BAJA / INFORMATIVA] Exposición de Errores Internos, API Key en Fuente y Pérdida de IP Real tras `systemd-socket-proxyd`

* **Ubicación:**
  * Múltiples `http.Error(w, err.Error(), http.StatusInternalServerError)` en [main.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L537) (líneas `537`, `547`, `766`, `781`, `820`, `835`, `968`, `981`, `1008`, `1050`, `1082`, `1117`, `1155`).
  * [`queryLastFM`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/smartmix/smartmix.go#L181): clave de Last.fm hardcodeada en el código fuente.
  * [`write_port80_units` / `write_port443_units`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/setup-https-pi.sh#L109-L140): reenvío TCP hacia `127.0.0.1:4080/4443`.
* **Análisis técnico:**
  Devolver `err.Error()` directamente en respuestas `500` filtra rutas de disco y mensajes internos de SQLite/red al navegador. La clave de Last.fm en `smartmix.go` debería admitir sobrescritura vía variable de entorno (`LASTFM_API_KEY`). Por último, el proxy L4 `systemd-socket-proxyd` hace que toda petición vista en los logs de Go por puerto `80`/`443` parezca originarse en `127.0.0.1` o en el gateway del bridge Docker, dificultando auditorías o limitación de tasa por IP real (si se necesita en el futuro, usar `network_mode: host` o proxy L7 con `X-Forwarded-For` confiable, o Caddy/Nginx ligero).
* **Remediación recomendada:**
  Registrar `err` detalladamente por `log.Printf` en servidor y devolver mensajes genéricos al cliente HTTP; mover `LASTFM_API_KEY` a entorno con fallback seguro.

---

## 5. Auditoría de Robustez, Concurrencia y Estabilidad 24/7 (`ROB-01` a `ROB-07`)

### ROB-01 [ALTO] `compressionAndSecurityMiddleware` Corrompe Respuestas `304`/`204`/`HEAD`/`Range` y Recomprime Carátulas

* **Ubicación:** [`gzipResponseWriter` y `compressionAndSecurityMiddleware`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L355-L402)
* **Análisis técnico:**
  El middleware asigna `w.Header().Set("Content-Encoding", "gzip")`, borra `Content-Length` y abre `gz.Reset(w)` + `defer gz.Close()` **antes** de llamar a `next.ServeHTTP(gzw, r)`. Esto causa cuatro problemas reales:
  1. En peticiones `HEAD`, `204 No Content`, `304 Not Modified` (habituales en `http.FileServer` al recargar `./web` o `/api/covers/`), o respuestas parciales `206 Partial Content`, el `defer gz.Close()` escribe siempre la cabecera y cola gzip (10+8 bytes) en el cuerpo, violando el protocolo HTTP e impidiendo cachés limpias en algunos clientes y proxies.
  2. Recomprime imágenes JPEG/PNG/WebP bajo `/api/covers/` en cada descarga sin beneficio de tamaño, consumiendo CPU innecesariamente en Raspberry Pi.
  3. Si un handler responde con `http.Error` enviando texto o cabeceras estándar sin pasar por comprobación previa de estado, tampoco implementa `Unwrap() http.ResponseWriter` (necesario en Go 1.20+ para que `http.NewResponseController` acceda al writer subyacente a través de middlewares).
* **Remediación recomendada:**
  Aplicar compresión condicional de manera diferida al escribir el primer byte (omitiéndola en `HEAD`, `204`, `304`, respuestas ya comprimidas o `Content-Type` binario como `image/*` y `audio/*`) e implementar `Unwrap() http.ResponseWriter`.

```go
// Unwrap expone el ResponseWriter subyacente para compatibilidad con http.NewResponseController.
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}
```

---

### ROB-02 [ALTO] `SelectAudioOutput` Puede Dejar Todas las Salidas Físicas Desactivadas si Falla el DAC

* **Ubicación:** [`SelectAudioOutput`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/mpd/client.go#L2755-L2769)
* **Análisis técnico:**
  Al cambiar entre salidas (p. ej. pasar del Jack 3.5mm a un DAC USB o I2S HAT recién conectado), el bucle ejecuta `enableoutput <id>` sobre el destino y acto seguido ejecuta `_, _ = c.sendRawCommand(fmt.Sprintf("disableoutput %d\n", o.ID))` sobre **todas las demás salidas físicas**, incluso si `enableoutput` devolvió error (`enableErr != nil`).
* **Impacto:**
  Si el DAC elegido falla al abrir el dispositivo ALSA (cable desconectado o formato no soportado), la salida anterior también queda desactivada y AuraBox deja de reproducir audio por hardware hasta que el usuario reseleccione manualmente otra salida válida.
* **Remediación recomendada:**
  Verificar `enableErr == nil` antes de desactivar el resto de salidas físicas; si falla la activación, abortar los `disableoutput`, resincronizar el estado real desde MPD y devolver el error al usuario.

---

### ROB-03 [ALTO] Desincronización de `config/mpd.conf`, Rutas USB Host/Contenedor y Nombres ALSA sin Escapar

* **Ubicación:**
  * [docker-compose.yml](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L49) (montaje `./music/usb:/music/usb:ro`), [línea 82](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker-compose.yml#L82) (`./config/mpd.conf:/etc/mpd.conf:ro`)
  * [docker/mpd/entrypoint.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/entrypoint.sh#L80) (genera `/tmp/mpd.runtime.conf`), [línea 227](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/entrypoint.sh#L227) (`name "${DISPLAY_NAME}"`) y [línea 273](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/docker/mpd/entrypoint.sh#L273)
  * [scripts/automount-usb.sh](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/scripts/automount-usb.sh#L9) (`MOUNT_BASE="/music/usb"`)
* **Análisis técnico:**
  1. `docker-compose.yml` monta `./config/mpd.conf:/etc/mpd.conf:ro`, pero `entrypoint.sh` arranca MPD apuntando exclusivamente a `/tmp/mpd.runtime.conf`. Cualquier cambio manual del usuario en `config/mpd.conf` queda silenciosamente ignorado.
  2. `automount-usb.sh` monta los pendrives USB bajo `/music/usb/...` (raíz del host), mientras que `docker-compose.yml` monta `./music/usb:/music/usb:ro` (carpeta relativa dentro del repositorio) y además sin propagación `rslave`. Resultado: los pendrives montados por `udev` no aparecen dentro de los contenedores salvo que coincida la ruta o se propague el montaje.
  3. En `entrypoint.sh:227`, `${DISPLAY_NAME}` procede de `aplay -l`. Si un dispositivo USB posee comillas dobles `"` en su descriptor USB, puede romper la sintaxis de `mpd.runtime.conf` e impedir el arranque de MPD.
* **Remediación recomendada:**
  * Escapar o eliminar `"` y `\` de `${DISPLAY_NAME}` antes de escribir `/tmp/mpd.runtime.conf`.
  * Alinear `MOUNT_BASE` en `automount-usb.sh` (por ejemplo `/var/lib/aurabox/usb` o la ruta configurada del repo) y montar dicho directorio con propagación `bind-propagation: rslave` (sintaxis larga de volumen en Compose) en ambos contenedores.
  * Documentar claramente en `config/mpd.conf` que actúa como plantilla/referencia y que la configuración activa la genera `entrypoint.sh` (o leer overrides concretos desde variables de entorno).

---

### ROB-04 [MEDIA] Acumulación de Pistas Huérfanas en SQLite y Bug `403 Forbidden` en "Subir un nivel"

* **Ubicación:**
  * [`ScanLibrary`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/scanner.go#L114-L145)
  * [`renderFolderBrowser`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/web/js/views.js#L648-L701) y [`handleLibraryFolders`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L855-L858)
* **Análisis técnico:**
  1. **Pistas huérfanas:** `ScanLibrary` únicamente ejecuta inserciones/actualizaciones (`UpsertTrack`) y nunca elimina de `tracks` archivos borrados del disco o pendrives USB desconectados. Con el tiempo, la biblioteca acumula "pistas fantasma" que provocan errores al intentar reproducirlas (y obligan al salto por error `trackSkipCount`). *(Nota: la purga debe omitirse si un origen entero como `/music/nas` aparece vacío por desconexión temporal del NAS, evitando borrar favoritos o `play_count` por un corte de red).*
  2. **Error `403` al navegar arriba en Carpetas:** Cuando el usuario está en `/music/local`, el botón `← Subir un nivel` (`views.js:699`) calcula `parent = '/music'`, y las migas de pan (`:648-657`) crean un enlace clickable hacia `/music`. Sin embargo, [`isSafeMusicPath`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L420-L423) solo admite `/music/local`, `/music/nas` y `/music/usb`, rechazando `/music` con HTTP `403 Forbidden` en vez de volver a la raíz virtual (`path = ""`).
* **Remediación recomendada:**
  En `views.js`, cuando `parent` es `/music` (o cualquier prefijo superior a las 3 raíces), navegar a `""`. En `scanner.go`, implementar conciliación segura que marque o pode rutas desaparecidas dentro de una raíz cuando dicha raíz exista y haya completado un escaneo íntegro sin errores de E/S.

---

### ROB-05 [MEDIA] Desfase de Códecs en Metadatos, Desborde `uint32` en VBR MP3 y Regex por Llamada

* **Ubicación:**
  * [`readAudioMetadata`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/metadata.go#L40-L53), [tasas MP3](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/metadata.go#L460-L476), [cálculo VBR](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/metadata.go#L504-L520)
  * [`scanDirectory`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/scanner/scanner.go#L241-L251)
  * [`cleanSongTitle` / `cleanArtistName` / `parseLRC`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/lyrics/lrclib.go#L72-L310)
* **Análisis técnico:**
  1. `scanner.go` y `main.go` indexan y muestran `.ogg`, `.m4a`, `.opus`, `.aac`, `.alac`, `.wma`, pero `metadata.go` devuelve `nil` para todos ellos (solo procesa `.flac`, `.mp3`, `.wav`), dejándolos sin artista, álbum ni duración en la interfaz.
  2. En el cálculo de duración VBR (`metadata.go:504, 520`), `uint32(frameCount) * uint32(samplesPerFrame)` multiplica en 32 bits antes de convertir a `float64`; en archivos muy largos (sesiones DJ / podcasts de varias horas) supera `2^32` muestras y da duración errónea por wrap-around. Además, solo contempla `layerBits == 1` (Layer III), devolviendo bitrate `0` en Layer I/II.
  3. `internal/lyrics/lrclib.go` invoca `regexp.MustCompile(...)` seis veces en tiempo de ejecución por cada petición de letras (`:72, :76, :80, :87, :89, :310`), generando asignaciones y trabajo de GC innecesarios.
* **Remediación recomendada:**
  Convertir a `float64` o `uint64` antes de multiplicar en `metadata.go` (`float64(frameCount) * float64(samplesPerFrame)`), precompilar las expresiones regulares como variables globales de paquete `var re... = regexp.MustCompile(...)` en `lrclib.go`, y añadir soporte al menos de contenedores MP4/M4A (átomos `moov/mvhd/ilst`) y OGG/Opus.

---

### ROB-06 [MEDIA] Apagado HTTP Abrupto (`Close`) y Conexiones SSE sin Límite ni Timeout de Escritura

* **Ubicación:**
  * [Señales de apagado en `main.go`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main.go#L247-L252)
  * [`Subscribe` y `ServeHTTP`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/events/sse.go#L44-L169)
* **Análisis técnico:**
  1. Ante `SIGTERM`/`SIGINT` (reinicio de contenedor o Pi), `main.go` llama a `httpServer.Close()`, cerrando conexiones HTTP activas al instante en lugar de drenar peticiones breves mediante `Shutdown(ctx)` (reservando el cierre forzado solo para flujos largos SSE/audio tras un timeout de 5 segundos).
  2. `events.Hub` no acota el número total de clientes SSE simultáneos (`h.clients`) y elimina de forma permanente el deadline de escritura (`SetWriteDeadline(time.Time{})` en `sse.go:95`). Si un móvil entra en túnel o cambia de Wi-Fi a 4G dejando el socket TCP semiabierto hasta llenar el buffer del kernel, la goroutine de `ServeHTTP` queda bloqueada indefinidamente en `fmt.Fprintf` (`sse.go:165`).
* **Remediación recomendada:**
  Imponer un límite máximo razonable de clientes SSE concurrentes (ej. 64), fijar un `SetWriteDeadline(time.Now().Add(10 * time.Second))` antes de cada escritura en `ServeHTTP` (manteniendo `SetReadDeadline` sin corte) y usar `Shutdown(ctx)` coordinado al recibir señal de salida.

---

### ROB-07 [BAJA] Comodines `LIKE` Sin Escapar en SQLite y Cobertura de Tests de Seguridad

* **Ubicación:**
  * [`GetTracksByPathPrefix`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go#L792), [`Search`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go#L915), [`GetSimilarArtistsOffline`](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/internal/db/sqlite.go#L1346)
  * [main_test.go](file:///media/paspas/RocknRoll/NOBORRAR/ONEDRIVE/CLASES/000.ClasesActuales/00.PROYECTOS/2026/jukeboox3/main_test.go)
* **Análisis técnico:**
  Aunque las consultas SQLite son seguras contra SQLi al ir parametrizadas, una carpeta o nombre de artista/búsqueda que contenga literalmente `%` o `_` actúa como comodín dentro de `LIKE ?`. En `GetTracksByPathPrefix` podría emparejar subrutas inesperadas si un directorio contiene `_` o `%`. Asimismo, `main_test.go` cubre redirección HTTPS, radios y proxy `/api/stream`, pero aún carece de tests automáticos para validar `replay_gain`, contención de symlinks/rutas, rechazo de métodos `GET` mutadores y topes de volumen.
* **Remediación recomendada:**
  Escapar `\`, `%` y `_` usando `ESCAPE '\'` en todas las cláusulas `LIKE` y ampliar `main_test.go` con casos unitarios de seguridad.

---

## 6. Revisión de Infraestructura Docker y Host Raspberry Pi (Operación 24/7)

### 6.1. Endurecimiento de `docker-compose.yml`

Ejemplo defensivo manteniendo estrictamente los puertos del rango **4000** y el acceso audiófilo a `/dev/snd`:

```yaml
services:
  aurabox:
    # Mantener rango 4000; metrics/admin ligado solo a loopback
    ports:
      - "4080:4080"
      - "4443:4443"
      - "127.0.0.1:4081:4081"
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    logging:
      driver: "json-file"
      options:
        max-size: "10m"
        max-file: "3"

  mpd:
    # No publicar 4600 ni 4082 en 0.0.0.0: aurabox usa mpd_run/socket y red interna aurabox-net
    expose:
      - "6600"
      - "4082"
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    logging:
      driver: "json-file"
      options:
        max-size: "10m"
        max-file: "3"
```

### 6.2. Protección de Tarjeta microSD en Funcionamiento Continuo 24/7

* **Rotación de logs Docker:** Sin `max-size` / `max-file`, las salidas estándar de `aurabox-core` y `aurabox-mpd` crecen sin límite hasta llenar la partición raíz de la Raspberry Pi.
* **Reducción de escrituras innecesarias:** Una vez corregido **SEC-06**, el certificado TLS deja de reescribirse ante peticiones falsas; además, conviene configurar `log_level "warning"` o limitar verbosidad tras validar el hardware ALSA y activar `journald` en memoria (`Storage=volatile` o tope `SystemMaxUse=50M` en `/etc/systemd/journald.conf`).
* **Permisos de secretos:** Asegurar `chmod 600 .env` y `chown` adecuado en `./data/certs/*.key`.

### 6.3. Firewall Host (`ufw`) y Acceso Remoto Seguro

* En la Raspberry Pi, habilitar `ufw` permitiendo únicamente SSH (`22/tcp`, preferiblemente limitado con llaves SSH) y los puertos estrictamente usados por la PWA desde la subred doméstica (por ejemplo `192.168.1.0/24`), bloqueando acceso externo directo a `4081`, `4082` y `4600`.
* **Nunca abrir puertos de AuraBox en el router hacia Internet.** Para escuchar o controlar AuraBox fuera de casa, utilizar una VPN privada como **WireGuard** o **Tailscale**.

---

## 7. Plan de Acción y Mejoras Sugeridas (Priorizado)

### Fase A — Quick Wins Críticos (Implementación Inmediata)

1. **[SEC-01]** Añadir lista blanca estricta (`off | track | album | auto`) a `replay_gain` en `main.go` y `mpd/client.go`, y validar rango de `crossfade`.
2. **[SEC-02]** Retirar el mapeo público `4600:6600` y `4082:4082` en `docker-compose.yml`, ligando además `4081` a `127.0.0.1:4081`.
3. **[SEC-03]** Exigir método `POST` mediante `requireMethod` en todos los handlers mutadores de reproductor, cola, escaneo y favoritos.
4. **[SEC-05 + ROB-03]** Sanitizar `ID_FS_LABEL` en `automount-usb.sh`, montar con `ro,nosuid,nodev,noexec`, alinear ruta USB con propagación `rslave`, corregir fallo en redirección HTTPS de `curl` y limpiar `" ` de nombres ALSA en `entrypoint.sh`.
5. **[ROB-01 + ROB-04]** Corregir el middleware gzip (`Unwrap()` + bypass en `304`/`204`/`HEAD`/`image/*`/`audio/*`) y el botón `← Subir un nivel` en Carpetas cuando el padre sea `/music`.

### Fase B — Endurecimiento LAN, Navegador y Contenedores (Corto Plazo)

1. **[SEC-06]** Restringir `EnsureHost` a IPs de interfaz reales o `TLS_EXTRA_HOSTS` con tope de SANs, y asegurar serial `>= 1` en X.509.
2. **[SEC-03]** Sustituir `Access-Control-Allow-Origin: *` por validación same-origin (`Origin == Host`) o lista blanca LAN explícita.
3. **[SEC-08]** Implementar `sanitizeCoverURL`, eliminar atributos `onerror=` inline mediante escucha en fase captura, reemplazar `innerHTML` en `lyrics.js:85` por `textContent` y añadir cabecera `Content-Security-Policy`.
4. **[SEC-09 + SEC-10]** Reescribir `isSafeMusicPath` con `EvalSymlinks` + `filepath.Rel`, acotar copias de carátulas a ficheros regulares de imagen (`<= 10 MB`), añadir `http.MaxBytesReader` en endpoints JSON y `io.LimitReader` + topes de tamaño en cachés `coverCache`/`lyrics.cache`.
5. **[SEC-04 + SEC-11]** Pasar ambos contenedores a usuario no privilegiado (`cap_drop: ALL`, `no-new-privileges:true`, versiones fijas Alpine/Go), poner lista blanca en `POST /api/settings`, retirar `nas_pass` redundante de SQLite y aplicar `chmod 600 .env`.

### Fase C — Robustez Audiófila y Calidad Continua (Medio Plazo)

1. **[SEC-07]** Proteger `ResolveStreamURLs` frente a SSRF validando IPs públicas tras resolución DNS antes de abrir el socket TCP y validando `LogoURL`.
2. **[ROB-02]** Añadir rollback o verificación previa en `SelectAudioOutput` para no dejar al sistema sin salida física activa ante fallos del DAC.
3. **[ROB-04 + ROB-05]** Conciliar y purgar pistas desaparecidas de forma segura tras escaneos limpios (sin borrar pistas si un recurso NAS está temporalmente caído), precompilar regex en `lrclib.go`, corregir multiplicación `uint32` en MP3 VBR e incorporar metadatos `.m4a`/`.ogg`/`.opus`.
4. **[ROB-06 + ROB-07]** Implementar `Shutdown(ctx)` ordenado, límites y timeouts de escritura en SSE, escape de comodines `LIKE` en SQLite y ampliar `main_test.go` con batería de tests de seguridad y regresión.

### Fase D — Sugerencias de Evolución Funcional y Arquitectura (Largo Plazo)

1. **Modo "Fiesta / Kiosko" vs. Modo Administrador (RBAC ligero con PIN):**  
   Permitir definir opcionalmente un PIN de administrador (`AURABOX_PIN`) mediante cookie `HttpOnly; SameSite=Strict`: cualquier invitado en el salón puede ver la biblioteca y encolar canciones (`Encolar siguiente`), mientras que cambiar el tope de volumen (`max_volume_limit`), alterar ajustes del DAC/NAS o borrar la cola queda protegido tras el PIN.
2. **Copias de seguridad automáticas de SQLite (`aurabox.db`):**  
   Programar una tarea ligera periódica en Go usando `VACUUM INTO '/app/data/backups/aurabox-YYYYMMDD.db'` (operación atómica compatible con WAL mientras el sistema suena) conservando los últimos 7 días, protegiendo así favoritos, contadores de reproducción y radios personalizadas frente a fallos eléctricos en la Pi.
3. **Observabilidad real en `:4081`:**  
   Hacer que `/health` verifique activamente `SELECT 1` en SQLite y ping al socket `/run/mpd/socket` (devolviendo `503` si algún componente crítico falla, lo que habilita `HEALTHCHECK` real en Docker), y exponer métricas en formato estándar OpenMetrics/Prometheus en `/metrics`.
4. **Solución definitiva del flujo NAS en UI:**  
   Hoy el formulario de Ajustes NAS guarda los datos en SQLite mientras que Docker monta CIFS estáticamente desde `.env`. Como mejora limpia de arquitectura, o bien documentar/convertir ese panel en un asistente de estado de solo lectura + generador seguro, o delegar el remontaje a un helper mínimo privilegiado en el host controlado estrictamente por parámetros validados.
5. **Validación automática en CI:**  
   Añadir ejecución periódica de `go test -race ./...`, `govulncheck ./...` (verificación de CVEs en Go stdlib y `go-sqlite3`) y linter estático `gosec` en el repositorio antes de construir imágenes para Raspberry Pi.
