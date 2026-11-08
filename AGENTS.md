Actúa como Ingeniero de Software Principal y Diseñador UX/UI Senior de Sistemas de Audio Embebidos en Linux.

Vamos a desarrollar desde cero un sistema completo, ultra-rápido, estéticamente impecable y profesional para convertir una Raspberry Pi (3B+, 4 o 5) en la mejor Jukebox/Reproductor de música de alta fidelidad, denominado internamente **"AuraBox"**.

---

### REGLA DE INFRAESTRUCTURA Y DESPLIEGUE (DOCKER)
1. **Containerización Total:**
   - La aplicación debe desplegarse mediante `docker-compose.yml`.
   - **Rango de puertos:** Usar estrictamente puertos en el rango 4000 para no colisionar con servicios comunes (puerto web principal: **4080**; métricas/admin opcional: **4081**).
2. **Acceso a Hardware y Audio Host:**
   - Acceso al bus de audio del host: `devices: ["/dev/snd"]` y grupo `audio`.
   - Conexión con MPD: Puede correr en contenedor dedicado o conectarse al MPD/CamillaDSP del host mediante `/run/mpd/socket` o red interna.
   - Soportar conmutación entre salidas de audio: Jack analógico integrado 3.5mm (`snd_bcm2835`), DACs USB (`hw:X,Y`) y DACs I2S HAT.
3. **Puntos de Montaje de Almacenamiento:**
   - Montar volúmenes para `/music/local` (música local), `/music/nas` (recursos de red SMB/NFS montados en el host) y `/music/usb` (unidades USB extraíbles detectadas).

---

### UX MÓVIL OBLIGATORIA (THUMB-FIRST & MOBILE-FIRST)
La interfaz es una PWA responsiva instalable, optimizada primordialmente para control con una sola mano desde smartphone (360px a 430px de ancho).

1. **Jerarquía Visual y Navegación:**
   - **Barra de navegación inferior fija (Dock):** 5 secciones clave: `Biblioteca`, `Buscar`, `Mixes`, `Cola`, `Ajustes`. Targets táctiles ≥ 48px en la zona del pulgar.
   - **Mini-Player Persistente Flotante:** Justo encima del dock inferior, visible en el 100% de las vistas si hay reproducción activa. Muestra carátula, título, artista, botón Play/Pausa y barra de progreso fina.
   - **Gestos Táctiles:**
     * Tap en Mini-Player → Abre vista **Now Playing** a pantalla completa.
     * Swipe Down en Now Playing → Vuelve a la vista previa.
     * Swipe Horizontal en Mini-Player → Pasa de canción (Next / Previous).
   - **Now Playing Envolvente:** Carátula dominante en alta resolución, fondo adaptativo con desenfoque basado en el color dominante de la carátula, badges discretos con resolución técnica (ej. `FLAC 96kHz / 24-bit` o `MP3 320kbps`), acceso a letras en 1 toque.
   - **Índice Alfabético Vertical (A-Z Slider):** En el lateral derecho de las listas largas de artistas y álbumes para salto rápido sin fricción de scroll.
   - **Salvaguarda de Volumen:** Deslizador con atenuación progresiva y botones directos `+` y `-`. Límite de ganancia máxima en ajustes para evitar picos accidentales al 100%.

2. **Flujos Críticos de Uso (User Journeys):**
   - **«Suena en 10 segundos»:** Arranque frío → Sección "Añadido recientemente" o "Álbumes" → Tap en álbum → Comienza la reproducción (≤ 3 toques totales).
   - **«Mix con tres toques»:** Desde cualquier artista → Menú `⋯` → `Crear Smart Mix` → Comienza a sonar de inmediato.
   - **«Manos ocupadas»:** Soporte completo de la API nativa de navegación `MediaSession` (carátula, título, artista en pantalla de bloqueo del móvil y soporte de botones físicos de volumen/auriculares).
   - **«Multi-dispositivo en tiempo real»:** Sincronización instantánea (<150ms) entre navegadores/móviles usando Server-Sent Events (SSE) o WebSockets. Si un móvil cambia la cola, se pausa o sube el volumen, el cambio se refleja en tiempo real en los demás.
   - **Optimistic UI:** Feedback visual instantáneo (<100ms) ante cualquier pulsación táctil con actualización optimista antes de esperar la respuesta del servidor.

---

### ESPECIFICACIÓN DE ALCANCE POR FASES (ROADMAP V1)

#### Fase 1: Arquitectura Base, Contenedores y Hardware
- Configuración de `Dockerfile` y `docker-compose.yml` (Go para el backend de alto rendimiento con <50MB RAM, Node/Vite/Svelte o React con Tailwind CSS para el frontend servido estáticamente en puerto 4080).
- Integración con el motor de audio MPD (gestión de salidas Jack / DAC USB / I2S).
- Script de automontaje y re-escaneo para pendrives USB (`udev`) y soporte para carpetas compartidas NAS (SMB/NFS).

#### Fase 2: Gestión de Biblioteca y Almacenamiento Dinámico
- Base de datos SQLite local para indexar metadatos (ID3v2, Vorbis tags, APE).
- Detección de bibliotecas dinámicas: escucha de eventos de archivos o sondeo rápido que dispare eventos SSE `library-updated` al cliente sin requerir refresco manual.
- Vistas de biblioteca con filtros y ordenación: «Añadido recientemente», «Artistas», «Álbumes», «Géneros» y «Explorador de carpetas» (para navegar USB y NAS por directorios).
- Extracción y caché reactiva de carátulas (`folder.jpg`, tags incrustados y fallback estilizado para emisoras de radio).

#### Fase 3: Frontend PWA y Sistema de Cola
- Implementación de la navegación inferior fija, Mini-Player persistente, gestor de gestos táctiles y vista Now Playing completa.
- Sistema de cola interactivo con reordenamiento mediante Drag & Drop táctil y eliminación por deslizamiento lateral.
- Acciones rápidas en menú contextual de cada canción: «Reproducir ahora», «Reproducir siguiente», «Añadir al final de la cola».
- Historial de navegación robusto (soporte para el botón Atrás nativo de Android e iOS sin callejones sin salida).

#### Fase 4: Motor "Smart Mix", Recomendación y Letras
- **Smart Mix & Radio de Artista:**
  * Al solicitar un mix de un artista, consultar la API abierta de Last.fm / ListenBrainz para extraer artistas relacionados.
  * *Fallback Offline Inteligente:* Si no hay internet, calcular similitud local cruzando géneros coincidentes, décadas/años y tempo en la base de datos local SQLite.
  * Intercalar inteligentemente las canciones (40% canciones populares/favoritas del artista semilla + 60% temas destacados de artistas relacionados presentes en la biblioteca local).
  * Ordenación armónica sin saltos bruscos para garantizar que la sesión fluya.
- **Letras Sincronizadas:** Integración con la API abierta y gratuita **LRCLIB** (sin API key requerida) para mostrar letras sincronizadas desplazables línea por línea en Now Playing.
- **Favoritos y Estadísticas:** Sistema de marcado de favoritos (♥) y contador de reproducciones persistente (usando stickers de MPD o tablas SQLite).

#### Fase 5: Conectividad y Fuentes Externas
- Módulo de administración de almacenamiento en red (conexión simplificada a NAS vía SMB con guardado de credenciales).
- Integración de streaming de emisoras de radio web (.m3u/.pls) con logotipos e información de metadatos dinámica.

#### Fase 6: Audio Hi-Fi y Experiencia de Reproducción
- **Reproducción Gapless** (sin pausas) y **Crossfade inteligente** configurable en segundos.
- **Normalización de Volumen:** Activación de **ReplayGain** nativo (modo pista / modo álbum).
- **Shuffle Real Anti-repetición:** Algoritmo que baraja toda la colección sin repetir pistas del mismo artista consecutivamente.
- **Fade de Volumen:** Desvanecimiento suave (fade in/out de 1 a 2 segundos) al pausar o reanudar.
- **Sleep Timer (Temporizador de apagado):** Diálogo rápido con opciones de 15, 30, 45, 60 min o "al terminar la pista/álbum".

---

### ENTREGABLES REQUERIDOS:
1. **Configuración Docker:** `docker-compose.yml` mapeando puertos `4080:4080`, volúmenes de música y `/dev/snd`.
2. **Esquema de Base de Datos y Backend (Go):** Estructura del servidor, modelos SQLite (`tracks`, `artists`, `albums`, `stats`), controlador de MPD y endpoints SSE.
3. **Módulo Smart Mix:** Código en Go con el algoritmo de selección y cruce entre la API de similitud y el catálogo local.
4. **Componentes Frontend Clave:** Código del Dock inferior, Mini-Player con gestos táctiles (Swipe-up/horizontal) y vista Now Playing con letras LRCLIB integradas.