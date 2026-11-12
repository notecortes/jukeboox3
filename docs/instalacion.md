# Guía de Instalación y Despliegue de AuraBox

**AuraBox** es un sistema embebido de audio audiófilo y Jukebox inteligente de alto rendimiento (<50MB RAM), totalmente containerizado en **Docker** y diseñado con una arquitectura moderna en **HTML5, CSS3 y JavaScript vanilla** (sin Node.js, Angular, React ni Tailwind).

Esta guía cubre:
1. **Despliegue e Instalación en una Raspberry Pi (3B+, 4 o 5)** conectada al NAS y al amplificador.
2. **Uso en Cualquier Otro Ordenador con Docker** (Linux, Windows o macOS).
3. **Configuración de Salidas de Audio Hi-Fi (USB DAC, I2S HAT y Jack) y Radios Web.**
4. **Acceso Directo por IP (HTTPS con Certificado CA o Modo Solo HTTP).**
5. **Mantenimiento y Solución de Problemas.**

---

## 🏗️ Puertos y Arquitectura de Red

AuraBox utiliza estrictamente puertos en el rango **4000** para garantizar cero colisiones con otros servicios:

| Puerto | Protocolo | Servicio | Descripción |
|---|---|---|---|
| **4080** | HTTP | PWA / API / Redirect | Interfaz web HTTP (y redirección automática a HTTPS si entra por puerto 80 o `FORCE_HTTPS=true`). |
| **4443** | HTTPS | PWA / API TLS | Interfaz web segura HTTPS con certificado SAN automático y descarga de CA local (`/api/security/ca.crt`). |
| **4081** | HTTP | Administración | Panel de métricas del sistema, comprobación de estado de salud (`/health`, `/metrics`). |
| **4082** | HTTP | Hi-Fi Web Stream | Flujo de audio MP3 320kbps / FLAC para escuchar la música directamente en el navegador del móvil o portátil. |
| **4600** | TCP | MPD Control | Puerto de control MPD mapeado externamente (aislado del puerto estándar 6600). |

---

## 🍓 PARTE 1: Instalación en una Raspberry Pi (3B+, 4 o 5)

La Raspberry Pi actúa como el núcleo de audio dedicado conectado físicamente a tu amplificador estéreo o equipo de alta fidelidad, funcionando 24/7 de forma silenciosa y eficiente.

### 1.0. Preparar la Tarjeta microSD desde Cero (Sistema Operativo)

Si partes de una tarjeta microSD vacía o recién formateada:

1. **Descarga Raspberry Pi Imager** en tu ordenador (disponible gratis en [raspberrypi.com/software](https://www.raspberrypi.com/software/) para Linux, Windows y macOS).
2. **Elige las opciones en Raspberry Pi Imager:**
   * **Dispositivo:** Tu modelo de Raspberry Pi (`3`, `4` o `5`).
   * **Sistema Operativo:** `Raspberry Pi OS (other)` → **`Raspberry Pi OS Lite (64-bit)`** *(recomendado: sin entorno gráfico pesado, deja toda la RAM libre para audio y Docker)*.
   * **Almacenamiento:** Selecciona tu tarjeta microSD.
3. **Configuración previa (*Editar ajustes / OS Customisation*):** *(muy recomendado para no necesitar monitor ni teclado)*
   * **Hostname:** `aurabox` (luego podrás conectar como `aurabox.local`).
   * **Usuario y contraseña:** Define tu usuario de acceso (ej. `pi`) y su contraseña.
   * **Wi-Fi (si no usas cable Ethernet):** Introduce tu red Wi-Fi y contraseña, y selecciona el país (`ES`).
   * **Servicios → Activar SSH:** Marca **Usar autenticación por contraseña**.
4. **Graba la tarjeta**, insértala en la Raspberry Pi y enciéndela. Espera ~60 segundos al primer arranque y entra desde tu PC por SSH:
   ```bash
   ssh pi@aurabox.local
   ```

### 1.1. Conexión de Hardware al Amplificador

Elige una de las siguientes tres opciones de audio según tu equipo:

```
                  ┌──────────────────────┐
                  │    Raspberry Pi      │
                  │   (3B+ / 4 / 5)      │
                  └──────────┬───────────┘
                             │
     ┌───────────────────────┼────────────────────────┐
     │ Opción A: USB         │ Opción B: I2S HAT      │ Opción C: Analógico
     ▼                       ▼                        ▼
┌──────────────┐       ┌──────────────┐         ┌──────────────┐
│   DAC USB    │       │ Placa HAT    │         │ Jack 3.5mm   │
│ (Topping,    │       │ (HiFiBerry,  │         │ integrado de │
│  SMSL, etc.) │       │  Allo Boss)  │         │ la Pi        │
└──────┬───────┘       └──────┬───────┘         └──────┬───────┘
       │ Cables RCA           │ Cables RCA             │ Cable Jack 3.5mm a RCA
       └──────────────────────┼────────────────────────┘
                              ▼
                 ┌──────────────────────────┐
                 │   Amplificador Hi-Fi     │
                 │    (Entrada AUX / CD)    │
                 └────────────┬─────────────┘
                              ▼
                     [Altavoces Estéreo]
```

* **Opción A (Recomendada para Hi-Fi - DAC USB):** Conecta tu DAC USB (Topping, SMSL, Schiit, Cambridge Audio, etc.) a un puerto USB de la Raspberry Pi. Luego conecta cables RCA estéreo desde las salidas `LINE OUT` del DAC a una entrada de línea (`AUX`, `CD` o `TAPE`) de tu amplificador.
* **Opción B (DAC HAT I2S):** Si utilizas una tarjeta HAT montada sobre los pines GPIO (Dual ES9023, HiFiBerry DAC+, DAC2 HD, Allo Boss, IQaudio), conecta los cables RCA directamente desde la tarjeta HAT al amplificador y recuerda activar su `dtoverlay=` en `/boot/firmware/config.txt` (ver sección de problemas comunes en la **Parte 5**).
* **Opción C (Jack analógico 3.5mm integrado):** Conecta un cable con conector Jack 3.5mm en un extremo (a la salida de audio de la Raspberry Pi) y dos conectores RCA (blanco y rojo) al amplificador.

---

### 1.2. Método Rápido: Instalación Automatizada con Script

Hemos incluido un instalador desatendido que configura dependencias, Docker, grupos de usuario, permisos de sonido ALSA y el montaje del NAS en un solo paso:

1. **Instala Git y clona el proyecto en tu Raspberry Pi:**
   ```bash
   sudo apt update && sudo apt install -y git
   git clone https://github.com/notecortes/jukeboox3.git ~/aurabox
   cd ~/aurabox
   ```

2. **(Opcional) Preconfigura el acceso a tu NAS antes de instalar:**
   ```bash
   cp .env.example .env
   nano .env
   ```

3. **Otorga permisos y ejecuta el instalador:**
   ```bash
   chmod +x scripts/install-rpi.sh
   ./scripts/install-rpi.sh
   ```

4. **Qué realiza automáticamente el instalador:**
   * Actualiza el sistema e instala `cifs-utils`, `alsa-utils` y `curl`.
   * Instala Docker Engine y Docker Compose si no están presentes.
   * Añade el usuario a los grupos de sistema `docker` y `audio` (imprescindible para el bus de sonido `/dev/snd`).
   * Configura las credenciales de tu NAS en el archivo `.env`.
   * Aplica reglas `udev` para detección y montaje instantáneo de pendrives USB.
   * Compila y levanta los contenedores con reinicio automático ante reinicios o cortes de luz.

---

### 1.3. Método Manual en Raspberry Pi (Paso a Paso)

Si prefieres realizar la configuración paso a paso:

#### Paso 1: Instalar Docker y utilidades
```bash
sudo apt update && sudo apt install -y curl cifs-utils alsa-utils git
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker,audio $USER
newgrp docker
```

#### Paso 2: Clonar el repositorio
```bash
git clone https://github.com/notecortes/jukeboox3.git ~/aurabox
cd ~/aurabox
```

#### Paso 3: Configurar las variables del NAS
Copia la plantilla `.env.example` a `.env` (el archivo `.env` está protegido por `.gitignore` para no subir nunca tus contraseñas) y añade los datos de acceso de tu servidor de red o disco compartido:
```bash
cp .env.example .env
nano .env
```
Contenido del archivo `.env`:
```ini
# Puertos internos Docker (rango 4000) y opciones HTTPS
PORT=4080
HTTPS_PORT=4443
HTTPS_PUBLIC_PORT=4443
FORCE_HTTPS=false
TLS_EXTRA_HOSTS=
ADMIN_PORT=4081

# Configuración del servidor NAS (SMB / CIFS)
NAS_ALIAS=mi-nas
NAS_HOST=192.168.1.100
NAS_SHARE=music
NAS_USER=tu_usuario_nas
NAS_PASS=TuContraseñaDelNAS
NAS_OPTIONS=vers=3.0,sec=ntlmssp,ro,iocharset=utf8
```
*(Si posteriormente ejecutas `./scripts/setup-https-pi.sh`, el propio script ajustará automáticamente `HTTPS_PUBLIC_PORT=443`, `FORCE_HTTPS=true` y `TLS_EXTRA_HOSTS` con la IP de tu Raspberry Pi sin sobrescribir tus credenciales del NAS).*

#### Paso 4: Arrancar AuraBox
```bash
# En Raspberry Pi se recomienda limitar la compilación en paralelo para evitar picos de RAM:
sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build
```

#### Paso 5: Comprobar que los contenedores están activos
```bash
docker compose ps
```
Deberás ver activos:
* `aurabox-core`: Servidor web, API Go, SSE y escáner de biblioteca.
* `aurabox-mpd`: Motor de reproducción directa por hardware y transmisión web.

---

## 💻 PARTE 2: Uso en Otro Ordenador con Docker

AuraBox es totalmente portable y puede ejecutarse en cualquier ordenador de sobremesa o portátil con **Linux**, **Windows** o **macOS**.

### 2.1. Despliegue en Linux (Ubuntu, Debian, Fedora, Arch)

En Linux, los contenedores acceden directamente a tu tarjeta de sonido interna o DAC USB mediante `/dev/snd`.

1. **Clona el proyecto y entra en la carpeta:**
   ```bash
   git clone https://github.com/notecortes/jukeboox3.git ~/aurabox
   cd ~/aurabox
   ```
2. **Asegúrate de que tu usuario pertenece al grupo de sonido:**
   ```bash
   sudo usermod -aG audio $USER
   ```
3. **Configura el archivo `.env`:**
   ```bash
   cp .env.example .env
   nano .env
   ```
   Si la música está en una carpeta local de tu disco duro, puedes mapearla en `docker-compose.yml`:
   ```yaml
   volumes:
     - /home/usuario/Musica:/music/local:ro
     - nas_music:/music/nas:ro
   ```
   Si accedes al NAS, indica la IP y credenciales en `.env`.
4. **Inicia el sistema:**
   ```bash
   docker compose up -d --build
   ```
5. **Abre tu navegador en:**
   `http://localhost:4080`

---

### 2.2. Despliegue en Windows (Docker Desktop con WSL2)

En Windows, Docker se ejecuta sobre el subsistema WSL2. 

1. **Instalar requisitos:**
   * Instala **Docker Desktop para Windows** con el backend WSL2 habilitado.
2. **Clona el proyecto:**
   Abre una terminal de PowerShell o WSL2 y clona el repositorio:
   ```powershell
   git clone https://github.com/notecortes/jukeboox3.git C:\aurabox
   cd C:\aurabox
   copy .env.example .env
   ```
3. **Mapeo de tu música local:**
   En Windows, edita `docker-compose.yml` para apuntar a tu carpeta de música de Windows (por ejemplo, `C:\Users\TuUsuario\Music`):
   ```yaml
   volumes:
     - "C:/Users/TuUsuario/Music:/music/local:ro"
     - ./data:/app/data
   ```
4. **Iniciar AuraBox:**
   ```powershell
   docker compose up -d --build
   ```
5. **Reproducción de sonido en Windows (Modo Transmisión Web):**
   * Debido a que Docker Desktop en Windows no tiene acceso directo a la tarjeta de sonido ALSA de la placa base, AuraBox incluye el modo **«Transmisión Web HTTP (Hi-Fi Stream)»** integrado en el puerto `4082`.
   * En la interfaz web (`http://localhost:4080`), pulsa en la cabecera superior sobre el botón **«📻 Escuchar Móvil / Web»** o selecciona la salida de streaming. El audio sonará directamente por los altavoces de Windows a través del propio navegador con máxima fidelidad (320kbps MP3 sin pérdidas apreciables).

---

### 2.3. Despliegue en macOS (Apple Silicon M1/M2/M3 o Intel)

1. **Instalar Docker Desktop para Mac**.
2. **Clona el proyecto:**
   ```bash
   git clone https://github.com/notecortes/jukeboox3.git ~/aurabox
   cd ~/aurabox
   cp .env.example .env
   ```
3. **Configurar la carpeta de música:**
   En `docker-compose.yml`, vincula tu carpeta de música de Mac:
   ```yaml
   volumes:
     - "/Users/tuusuario/Music:/music/local:ro"
   ```
4. **Levantar los contenedores:**
   ```bash
   docker compose up -d --build
   ```
5. **Reproducción:**
   Abre `http://localhost:4080`. Puedes escuchar la música por los altavoces de tu Mac activando el botón de **Transmisión Web**.

---

## 📱 PARTE 3: Control desde Smartphones y Tablets (PWA)

AuraBox está optimizado con ergonomía **Thumb-First** para manejarse cómodamente con una sola mano desde cualquier teléfono móvil conectado a la misma red WiFi.

1. **Averigua la IP local del equipo donde corre AuraBox:**
   * En la Raspberry Pi o Linux: `hostname -I` (ejemplo: `192.168.1.113`).
2. **Abre el navegador en tu smartphone:**
   * **Acceso estándar por puerto:** `http://192.168.1.113:4080` (HTTP) o `https://192.168.1.113:4443` (HTTPS).
   * **Acceso directo poniendo solo la IP:** `https://192.168.1.113` (con `./scripts/setup-https-pi.sh`) o `http://192.168.1.113` en modo solo HTTP sin certificados (`./scripts/setup-https-pi.sh --http`).
   > 💡 **Recomendado usar HTTPS si quieres instalar la PWA en el móvil:** Evita alertas de *«Página no segura»* al rellenar credenciales del NAS en **Ajustes** y habilita la instalación completa como **PWA** y controles en pantalla de bloqueo (`MediaSession`).
3. **Instalar como Aplicación Nativa (PWA):**
   * *(Si usas HTTPS con la CA local, instala primero `aurabox-ca.crt` como se detalla más abajo para que el navegador reconozca la conexión como de confianza total).*
   * **En Android (Chrome):** Toca los tres puntos de la esquina superior derecha y selecciona **«Añadir a pantalla de inicio»** o **«Instalar aplicación»**.
   * **En iPhone / iPad (Safari):** Toca el botón Compartir (icono con flecha hacia arriba) y selecciona **«Añadir a la pantalla de inicio»**.
4. **Características disponibles en el móvil:**
   * **Dock inferior ergonómico:** Acceso con el pulgar a Biblioteca, Búsqueda, Smart Mixes, Cola y Ajustes.
   * **Mini-Player persistente:** Tap para expandir a pantalla completa, deslizamiento horizontal para cambiar de canción.
   * **Letras sincronizadas en tiempo real (LRCLIB):** Toca el botón `LETRAS` en la pantalla de reproducción para seguir la letra línea por línea.
   * **Integración con la pantalla de bloqueo (MediaSession):** Controla la reproducción, salta de pista y visualiza la carátula sin necesidad de desbloquear el teléfono.

---

## 🎛️ PARTE 4: Configuración de Audio y Opciones Hi-Fi

Accede a la pestaña **Ajustes** en la aplicación para configurar:

1. **Selector de Salida en Caliente (Hardware + Hotplug):**
   * **Jack 3.5mm Analógico:** Para la salida de auriculares de la Raspberry Pi (`snd_bcm2835`).
   * **DAC USB Externo:** Máxima fidelidad Plug & Play mediante convertidor digital-analógico USB externo (`plughw:CARD=...`). Si enciendes o conectas el DAC después de arrancar AuraBox, pulsa **🔄 Redetectar DAC** en **Ajustes** (el sistema también lo detecta automáticamente en reposo).
   * **DAC I2S HAT:** Para tarjetas dedicadas de audio conectadas en GPIO (HiFiBerry, IQaudio, Allo Boss, etc.). *(En una microSD nueva con Raspberry Pi OS, requiere activar su línea `dtoverlay=` en `/boot/firmware/config.txt` y reiniciar la Pi).*
   * **Transmisión Web HTTP:** Para retransmitir la música a móviles o navegadores web remotos.
2. **Normalización ReplayGain:**
   * Modo `Pista` o `Álbum` para evitar saltos molestos de volumen entre distintas grabaciones.
3. **Crossfade Inteligente:**
   * Ajusta el fundido suave entre canciones de 0 a 15 segundos (0s para modo Gapless sin pausas).
4. **Salvaguarda de Volumen:**
   * Limita el volumen máximo (por ejemplo al 85%) para proteger tus altavoces y oídos de pulsaciones accidentales al 100%.

### 📻 Emisoras de Radio Web Hi-Fi (incluidas emisoras españolas)

AuraBox integra de fábrica un catálogo de **283 emisoras de radio por internet** listas para sonar en tu amplificador o en el navegador/móvil:

* **Filtro España (`🇪🇸 España`):** Incluye **50 emisoras españolas** nacionales y temáticas listas en 1 toque (RNE Radio Nacional, RNE Radio 3, RNE Radio Clásica, RNE Radio 5 Todo Noticias, Cadena SER, COPE Nacional, Onda Cero, LOS40, Rock FM España, Kiss FM España, Europa FM, Catalunya Ràdio, RAC1, Canal Sur Radio, EITB Radio Euskadi, entre otras).
* **Acceso rápido:** En **Biblioteca → Radios** o **Ajustes → Emisoras de Radio Web Hi-Fi**, donde puedes filtrar por país/género, buscar por nombre o abrir el catálogo completo.
* **Emisoras personalizadas (`.m3u` / `.pls` / `.m3u8` / stream directo):** Puedes añadir tus propias emisoras indicando nombre, género y URL (resuelve automáticamente listas `.m3u` / `.pls`) o eliminar emisoras personalizadas desde la interfaz o `POST / DELETE /api/radios`.
* **Estabilidad en directo:** Muestra en tiempo real el programa o canción emitida (`StreamTitle` vía ICY), reconecta automáticamente si la emisora sufre microcortes de red (hasta 3 reintentos) y usa `/api/stream` cuando navegas por HTTPS para escucharla también desde el móvil sin bloqueos de contenido mixto.

### 🔒 Acceso Directo poniendo solo la IP (HTTPS o Solo HTTP, sin escribir `:4080` ni `:4443`)

AuraBox incorpora un servidor **HTTPS nativo (TLS 1.2+) en el puerto `4443`**, manteniendo `docker-compose.yml` estrictamente dentro del rango `4000`:

* Al arrancar por primera vez, genera automáticamente en `./data/certs/` una **Autoridad Certificadora local persistente** (`aurabox-ca.crt` + `aurabox-ca.key`, válida 10 años) y un certificado de servidor ECDSA (`aurabox.crt` + `aurabox.key`, válido 800 días para cumplir el límite ≤ 825 días de iOS/macOS).
* Cada acceso por una nueva IP de tu LAN o nombre mDNS (`aurabox.local`) añade dinámicamente esa dirección a los **SAN** (*Subject Alternative Names*) del certificado sin reiniciar el servidor.
* Cuando navegas por **HTTPS**, el botón **📻 Móvil / Web** reproduce automáticamente el audio a través de `/api/stream` (mismo origen cifrado), evitando bloqueos por *Mixed Content* de `http://...:4082`.

#### Opción A: Acceso estándar en puertos `:4080` / `:4443` (sin tocar puertos del sistema)

1. Entra en `http://<IP_RASPBERRY>:4080` (HTTP) o `https://<IP_RASPBERRY>:4443` (HTTPS, o desde **Ajustes → Conexión Segura HTTPS → 🔒 Abrir en HTTPS**).

#### Opción B: Entrar poniendo únicamente la IP con HTTPS (`192.168.x.x` → `https://192.168.x.x`)

Si quieres teclear solo la IP en el navegador (o en el móvil) y que redirija automáticamente a HTTPS limpio en el puerto `443`:

```bash
cd ~/aurabox
git pull
chmod +x scripts/setup-https-pi.sh
./scripts/setup-https-pi.sh
```

**Qué realiza exactamente `scripts/setup-https-pi.sh`:**
1. Verifica que ningún otro servicio web (`nginx`, `apache2`, `caddy`, etc.) esté ocupando los puertos `80` o `443`.
2. Detecta la IP LAN (`hostname -I`) y `<hostname>.local`, y actualiza `.env` preservando tus credenciales del NAS:
   * `HTTPS_PORT=4443`
   * `HTTPS_PUBLIC_PORT=443`
   * `FORCE_HTTPS=true`
   * `TLS_EXTRA_HOSTS=<IP_LAN>,<hostname>.local,aurabox.local`
3. Configura cuatro unidades ligeras de `systemd` usando `/lib/systemd/systemd-socket-proxyd` (nativo en Debian/Raspberry Pi OS, sin instalar proxies externos y manteniendo Docker en `4080`/`4443`):
   * `aurabox-port80.socket` / `.service`: redirige `0.0.0.0:80` → `127.0.0.1:4080` (donde AuraBox emite `307 Temporary Redirect` hacia `https://<IP>/`).
   * `aurabox-port443.socket` / `.service`: redirige `0.0.0.0:443` → `127.0.0.1:4443` (pasarela TLS transparente).
4. Si `ufw` está activo, abre `80/tcp` y `443/tcp`.
5. Reconstruye y reinicia `aurabox` (`sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build aurabox`) y valida tanto la redirección `307` en el puerto `80` como la respuesta HTTPS en el puerto `443`.

#### Opción C: Entrar poniendo únicamente la IP en solo HTTP (`192.168.x.x` → `http://192.168.x.x`, sin HTTPS ni certificados)

Si prefieres no instalar ningún certificado CA y acceder simplemente escribiendo `http://<IP_RASPBERRY>` sin `:4080` ni redirecciones HTTPS:
```bash
./scripts/setup-https-pi.sh --http
```
*(Desactiva el puerto `443`, fija `HTTPS_PORT=off` y `FORCE_HTTPS=false` en `.env`, y activa únicamente `aurabox-port80.socket` hacia `127.0.0.1:4080`).*

#### Deshacer la redirección de puertos `80`/`443`
Para eliminar los sockets de los puertos `80` y `443` y volver a los puertos originales (`http://<IP>:4080` y `https://<IP>:4443`):
```bash
./scripts/setup-https-pi.sh --disable
```

> **Nota sobre caché PWA (Firefox/Chrome):** Si antes habías abierto `https://<IP_RASPBERRY>/` y después ejecutas `--http` o `--disable` (que cierran el puerto `443`), entra poniendo `http://` explícito o el puerto `:4080` / `:4443`. Si Firefox sigue intentando cargar una versión antigua en caché de `https://<IP_RASPBERRY>/`, abre `about:serviceworkers` en Firefox y pulsa **Cancelar el registro** en esa URL.

#### Cómo instalar el certificado `aurabox-ca.crt` para eliminar el aviso del navegador

Al tratarse de una IP privada de red doméstica (`192.168.x.x`), las autoridades públicas como Let's Encrypt **no pueden emitir certificados** para IPs privadas. Tienes dos posibilidades:
* **Uso rápido:** Aceptar una vez la advertencia inicial (*Avanzado → Continuar*); la conexión ya viajará cifrada y los formularios HTTP dejarán de alertar.
* **Recomendado (candado válido sin avisos e instalación PWA completa):** Descarga `aurabox-ca.crt` (exento de redirección para poder descargarlo incluso antes de confiar en HTTPS) desde **Ajustes → Conexión Segura HTTPS → 📜 Descargar Certificado CA** o en:
  * `http://<IP_RASPBERRY>/api/security/ca.crt` (o `:4080/api/security/ca.crt`)

Instálalo como **Entidad emisora raíz de confianza** en los dispositivos que uses habitualmente:

* **Android (Chrome):**
  1. Descarga `aurabox-ca.crt`.
  2. Ve a **Ajustes → Seguridad y privacidad → Más ajustes de seguridad → Cifrado y credenciales → Instalar un certificado → Certificado de CA** *(la ruta exacta puede variar según fabricante, busca «certificado» en Ajustes)*.
  3. Confirma *Instalar de todos modos*, selecciona `aurabox-ca.crt` y reinicia Chrome.
* **iPhone / iPad (iOS / iPadOS):**
  1. Abre el enlace de descarga desde **Safari** y pulsa **Permitir**.
  2. Ve a **Ajustes → Perfil descargado** *(o **General → VPN y gestión de dispositivos**)* → **Instalar** `AuraBox Local Root CA`.
  3. **Paso imprescindible en Apple:** Ve a **Ajustes → General → Información → Ajustes de confianza de certificados** y **activa la casilla** junto a `AuraBox Local Root CA`.
* **Linux (Chrome / Edge / Firefox / Sistema):**
  * **Chrome / Chromium / Brave / Edge:** `chrome://settings/certificates` → pestaña **Autoridades** → **Importar** `aurabox-ca.crt` → marca *«Confiar en este certificado para identificar sitios web»*.
  * **Firefox:** `about:preferences#privacy` → *Certificados → Ver certificados → Autoridades → Importar* → marca *«Confiar en esta CA para identificar sitios web»*.
  * **A nivel de sistema (Debian / Ubuntu / Raspberry Pi OS):**
    ```bash
    sudo cp aurabox-ca.crt /usr/local/share/ca-certificates/aurabox-ca.crt
    sudo update-ca-certificates
    ```
* **Windows (Edge / Chrome):**
  * Doble clic en `aurabox-ca.crt` → **Instalar certificado...** → elige *Usuario actual* o *Equipo local* → **Colocar todos los certificados en el siguiente almacén → Entidades de certificación raíz de confianza**.
* **macOS (Safari / Chrome):**
  * Doble clic en `aurabox-ca.crt` para abrir **Acceso a Llaveros** → añade a *Inicio de sesión* o *Sistema* → doble clic sobre `AuraBox Local Root CA` → despliega **Confiar** → *Al usar este certificado: **Confiar siempre***.

#### Variables de entorno TLS disponibles (`.env`) y Proxy Inverso externo

| Variable | Valor por defecto | Función |
|---|---|---|
| `HTTPS_PORT` | `4443` | Puerto TLS interno/Docker (`off` o `0` lo desactiva). |
| `HTTPS_PUBLIC_PORT` | `4443` | Puerto anunciado en las redirecciones (`443` al usar `setup-https-pi.sh` o proxy externo). |
| `FORCE_HTTPS` | `false` | Si es `true`, cualquier entrada HTTP (incluso `:4080`) redirige con `307` a HTTPS. |
| `TLS_EXTRA_HOSTS` | *(vacío)* | Lista CSV de IPs/hostnames extra a pre-incluir en el SAN del certificado. |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | *(vacío)* | Rutas opcionales dentro del contenedor si prefieres aportar un certificado externo (p. ej. Tailscale o dominio propio). |

> **Si ya tienes Nginx o Caddy ocupando los puertos `80`/`443` en la Raspberry Pi:**
> `./scripts/setup-https-pi.sh` se detendrá con aviso para no pisar tu servidor web actual. Si tu proxy ya gestiona el certificado TLS externo, fija `HTTPS_PORT=off` en `.env` (para que AuraBox delegue TLS en tu proxy sin redirigir) y reenvía hacia `http://127.0.0.1:4080`, o mantén `HTTPS_PORT=4443` y apunta el upstream a `https://127.0.0.1:4443`:
> ```nginx
> location / {
>     proxy_pass https://127.0.0.1:4443;
>     proxy_ssl_verify off;
>     proxy_set_header Host $host;
>     proxy_set_header X-Forwarded-Proto $scheme;
>     proxy_http_version 1.1;
>     proxy_buffering off;
>     proxy_read_timeout 3600s;
> }
> ```
> O en **Caddy** (con `HTTPS_PORT=off` en `.env`): `reverse_proxy 127.0.0.1:4080`.

#### Comprobación y diagnóstico de puertos directos y TLS

```bash
# 1. Ver estado de los sockets de redirección 80/443 en la Raspberry Pi
#    (En modo --http solo estará activo aurabox-port80.socket)
sudo systemctl status aurabox-port80.socket aurabox-port443.socket

# 2. Comprobar puerto 80:
#    - Modo HTTPS: debe devolver redirección 307 hacia https://
#    - Modo --http: debe devolver HTTP/1.1 200 OK
curl -I http://<IP_RASPBERRY>/

# 3. En modo HTTPS: consultar estado TLS, URL segura y SAN activos
curl -sk https://<IP_RASPBERRY>/api/security/status
```

---

## 🛠️ PARTE 5: Mantenimiento y Solución de Problemas

### Ver logs en tiempo real:
```bash
# Ver logs del núcleo AuraBox:
docker compose logs -f aurabox

# Ver logs del motor de audio MPD:
docker compose logs -f mpd
```

### Reiniciar los servicios:
```bash
docker compose restart
```

### Actualizar a la última versión:
```bash
cd ~/aurabox
git pull
sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build
```
*(En Raspberry Pi, `COMPOSE_PARALLEL_LIMIT=1` evita picos simultáneos de memoria al recompilar `aurabox` y `mpd`).*

### Problemas comunes:
* **No aparece el DAC USB o I2S HAT en la lista de salidas de Ajustes:**
  1. **Si usas un DAC USB externo:**
     * Comprueba que el DAC está encendido y bien conectado a un puerto USB de la Raspberry Pi (algunos DACs requieren alimentación propia o seleccionar entrada USB).
     * Pulsa el botón **🔄 Redetectar DAC** en **Ajustes → Salida de Audio Conmutable** (AuraBox usa mapeo dinámico `/dev/snd` + cgroup `116:*` para detectar DACs enchufados o encendidos en caliente sin reiniciar Docker).
  2. **Si usas una placa DAC I2S HAT sobre los pines GPIO (Dual ES9023, HiFiBerry, Allo Boss, IQaudio, InnoMaker, etc.):**
     * Tras grabar una microSD nueva con Raspberry Pi OS, Linux **no activa el driver I2S automáticamente** hasta declararlo en el gestor de arranque.
     * Edita el archivo de configuración de arranque en la Raspberry Pi:
       ```bash
       sudo nano /boot/firmware/config.txt
       ```
       Asegúrate de activar el bus I2S (`dtparam=i2s=on`) y añade al final la línea `dtoverlay=` exacta de tu placa, por ejemplo:
       * **DAC I2S Dual ES9023 / ESS Sabre / PCM5102A genérico:** `dtparam=i2s=on` y `dtoverlay=hifiberry-dac`
       * HiFiBerry DAC+ / Standard / InnoMaker DAC: `dtoverlay=hifiberry-dacplus`
       * HiFiBerry DAC2 HD: `dtoverlay=hifiberry-dacplushd`
       * IQaudio DAC+ / Pi-DAC PRO: `dtoverlay=iqaudio-dacplus`
       * Allo Boss DAC: `dtoverlay=allo-boss-dac-pcm512x-audio`
     * Reinicia la Raspberry Pi (`sudo reboot`) y verifica por SSH con `aplay -l` que aparece la nueva tarjeta antes de seleccionarla en **Ajustes**.
  3. Revisa debajo de las salidas el cuadro **«¿No aparece tu DAC?»**: AuraBox te mostrará exactamente las tarjetas ALSA (`card 0`, `card 1`…) que detecta el kernel dentro del contenedor.
* **Aviso de certificado «Tu conexión no es privada» o `setup-https-pi.sh` dice que el puerto `80`/`443` está ocupado:**
  1. Si el navegador muestra aviso de certificado en `https://<IP>`, es normal hasta instalar `aurabox-ca.crt` como autoridad de confianza en ese móvil u ordenador (consulta las instrucciones por sistema en la sección anterior).
  2. Si `setup-https-pi.sh` avisa de conflicto en puerto `80` o `443`, tienes otro servicio web en la Pi (por ejemplo Apache, Nginx o Pi-hole): libera esos puertos o añade un proxy inverso apuntando a `127.0.0.1:4080` / `127.0.0.1:4443`.
  3. Si usas iPhone/iPad, recuerda activar además **Ajustes → General → Información → Ajustes de confianza de certificados** tras instalar el perfil `AuraBox Local Root CA`.
* **No suena la música por el amplificador en Raspberry Pi:**
  1. Ve a **Ajustes** en la web (`https://<IP>`, `http://<IP>` con `--http`, o `http://<IP>:4080`) y verifica qué salida está seleccionada.
  2. Ejecuta en la terminal de la Raspberry Pi: `aplay -l` para verificar qué tarjetas ALSA reconoce Linux.
  3. Si usas Docker en la Pi y el DAC no aparecía al arrancar, pulsa **🔄 Redetectar DAC** o actualiza con `git pull && sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build`.
* **Error en Firefox (`Un ServiceWorker ha pasado un Error Response a FetchEvent.respondWith()`) tras desactivar HTTPS o cambiar puertos:**
  1. Al ejecutar `./scripts/setup-https-pi.sh --disable` (o `--http`), el puerto `443` deja de responder, mientras que una pestaña abierta en `https://<IP>/` podía conservar el Service Worker anterior en caché.
  2. Abre la dirección activa (`http://<IP>:4080` con `--disable`, `http://<IP>` con `--http`, o `https://<IP>:4443`).
  3. En Firefox, abre `about:serviceworkers` y pulsa **Cancelar el registro** sobre la URL antigua `https://<IP>/`. A partir de la caché `aurabox-v15`, AuraBox muestra además una tarjeta de recuperación con acceso en 1 toque a los puertos activos y limpieza automática del caché PWA.
* **Microcortes o bloqueos tras muchas horas de uso / reproducción en radios web:**
  1. AuraBox re-sincroniza automáticamente la cola si MPD se reinicia, reintenta hasta 3 veces las emisoras de radio en directo si cae el stream origen, y reconecta en segundo plano el canal SSE y `/api/stream` (sin cortar por timeout largo ni cachear flujos en `sw.js`).
  2. Si quieres verificar el estado interno de los contenedores tras un corte eléctrico o pérdida de DAC, consulta `http://<IP>:4081/health` y revisa `docker compose logs --tail=50 aurabox mpd`.
* **Error `permission denied` al montar el volumen del NAS (`nas_music`):**
  1. Si la contraseña del NAS contiene símbolos especiales (como `#`, `$` o espacios), envuélvela en comillas simples en `.env`: `NAS_PASS='tu_contraseña'`.
  2. Recuerda que Docker guarda las credenciales dentro del volumen una vez creado. Después de corregir `.env`, ejecuta:
     ```bash
     sudo docker compose down
     sudo docker volume rm aurabox_nas_music
     sudo docker compose up -d
     ```
  3. Verifica con `sudo mount -t cifs //<IP_NAS>/<SHARE> /mnt/test -o username=<USUARIO>,ro` que tu NAS no esté bloqueando la IP de la Raspberry Pi ni exigiendo otra versión de SMB (`vers=2.1` o sin `sec=ntlmssp` en `NAS_OPTIONS`).
* **La carpeta del NAS aparece vacía:**
  1. Revisa que las credenciales en el archivo `.env` (`NAS_USER`, `NAS_PASS`, `NAS_HOST`, `NAS_SHARE`) sean correctas.
  2. Comprueba que el NAS admite SMB versión 2.0 o 3.0.
  3. Puedes forzar un re-escaneo desde la aplicación tocando en **Ajustes > Re-escanear Biblioteca**.
* **Aviso `Your kernel does not support memory limit capabilities` en Raspberry Pi:**
  * Es solo informativo y no afecta a la reproducción. Si quieres habilitar el control de memoria en el kernel de la Pi, añade `cgroup_enable=memory cgroup_memory=1` al final de la línea en `/boot/firmware/cmdline.txt` y reinicia.



# Actualizar via GIT 

1 en el pc:
git add .gitignore docker-compose.yml docker/mpd/entrypoint.sh main.go main_test.go \
  internal/ scripts/ web/ docs/seguridad.md
git commit -m "Endurecimiento de seguridad y robustez 24/7"
git push origin main

2 en la raspberry
cd ~/aurabox
git pull origin main

# Proteger credenciales de tu .env actual (no se sobrescribe con git pull)
chmod 600 .env

# Actualizar el script udev USB mejorado (si usas pendrives automáticos)
if [ -f /usr/local/bin/aurabox-usb ]; then
  sudo cp scripts/automount-usb.sh /usr/local/bin/aurabox-usb
  sudo chmod +x /usr/local/bin/aurabox-usb
fi

# Reconstruir aurabox y mpd de uno en uno para cuidar la RAM de la Pi
sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build