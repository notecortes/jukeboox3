# Guía de Instalación y Despliegue de AuraBox

**AuraBox** es un sistema embebido de audio audiófilo y Jukebox inteligente de alto rendimiento (<50MB RAM), totalmente containerizado en **Docker** y diseñado con una arquitectura moderna en **HTML5, CSS3 y JavaScript vanilla** (sin Node.js, Angular, React ni Tailwind).

Esta guía cubre:
1. **Despliegue e Instalación en una Raspberry Pi (3B+, 4 o 5)** conectada al NAS y al amplificador.
2. **Uso en Cualquier Otro Ordenador con Docker** (Linux, Windows o macOS).
3. **Configuración de Salidas de Audio y Conectividad Hi-Fi**.
4. **Mantenimiento y Solución de Problemas**.

---

## 🏗️ Puertos y Arquitectura de Red

AuraBox utiliza estrictamente puertos en el rango **4000** para garantizar cero colisiones con otros servicios:

| Puerto | Protocolo | Servicio | Descripción |
|---|---|---|---|
| **4080** | HTTP | PWA / API / SSE | Interfaz web principal para móviles y PC, API REST y eventos en tiempo real (<50ms). |
| **4081** | HTTP | Administración | Panel de métricas del sistema, comprobación de estado de salud (`/health`, `/metrics`). |
| **4082** | HTTP | Hi-Fi Web Stream | Flujo de audio MP3 320kbps / FLAC para escuchar la música directamente en el navegador del móvil o portátil. |
| **4600** | TCP | MPD Control | Puerto de control MPD mapeado externamente (aislado del puerto estándar 6600). |

---

## 🍓 PARTE 1: Instalación en una Raspberry Pi (3B+, 4 o 5)

La Raspberry Pi actúa como el núcleo de audio dedicado conectado físicamente a tu amplificador estéreo o equipo de alta fidelidad, funcionando 24/7 de forma silenciosa y eficiente.

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
* **Opción B (DAC HAT I2S):** Si utilizas una tarjeta HAT montada sobre los pines GPIO (HiFiBerry DAC+, DAC2 HD, Allo Boss, IQaudio), conecta los cables RCA directamente desde la tarjeta HAT al amplificador.
* **Opción C (Jack analógico 3.5mm integrado):** Conecta un cable con conector Jack 3.5mm en un extremo (a la salida de audio de la Raspberry Pi) y dos conectores RCA (blanco y rojo) al amplificador.

---

### 1.2. Método Rápido: Instalación Automatizada con Script

Hemos incluido un instalador desatendido que configura dependencias, Docker, grupos de usuario, permisos de sonido ALSA y el montaje del NAS en un solo paso:

1. **Clona el proyecto en tu Raspberry Pi:**
   ```bash
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
# Configuración del servidor NAS (SMB / CIFS)
NAS_ALIAS=mi-nas
NAS_HOST=192.168.1.100
NAS_SHARE=music
NAS_USER=tu_usuario_nas
NAS_PASS=TuContraseñaDelNAS
```

#### Paso 4: Arrancar AuraBox
```bash
docker compose up -d --build
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
   * En la Raspberry Pi o Linux: `hostname -I` (ejemplo: `192.168.1.80`).
2. **Abre el navegador en tu smartphone:**
   Navega a `http://192.168.1.80:4080`.
3. **Instalar como Aplicación Nativa (PWA):**
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

1. **Selector de Salida en Caliente:**
   * **Jack 3.5mm Analógico:** Para la salida de auriculares de la Raspberry Pi (`snd_bcm2835`).
   * **DAC USB Externo (`hw:1,0`):** Máxima fidelidad mediante convertidor digital-analógico USB externo.
   * **DAC I2S HAT:** Para tarjetas dedicadas de audio conectadas en GPIO (HiFiBerry, IQaudio, etc.).
   * **Transmisión Web HTTP:** Para retransmitir la música a móviles o navegadores web remotos.
2. **Normalización ReplayGain:**
   * Modo `Pista` o `Álbum` para evitar saltos molestos de volumen entre distintas grabaciones.
3. **Crossfade Inteligente:**
   * Ajusta el fundido suave entre canciones de 0 a 15 segundos (0s para modo Gapless sin pausas).
4. **Salvaguarda de Volumen:**
   * Limita el volumen máximo (por ejemplo al 85%) para proteger tus altavoces y oídos de pulsaciones accidentales al 100%.

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
git pull
docker compose up -d --build
```

### Problemas comunes:
* **No suena la música por el amplificador en Raspberry Pi:**
  1. Ve a **Ajustes** en la web (`http://<IP>:4080`) y verifica qué salida está seleccionada.
  2. Si usas DAC USB, asegúrate de que el contenedor tiene acceso a `/dev/snd` (está configurado por defecto en `docker-compose.yml`).
  3. Ejecuta en la terminal de la Raspberry Pi: `aplay -l` para verificar que Linux reconoce tu DAC.
* **La carpeta del NAS aparece vacía:**
  1. Revisa que las credenciales en el archivo `.env` (`NAS_USER`, `NAS_PASS`, `NAS_HOST`, `NAS_SHARE`) sean correctas.
  2. Comprueba que el NAS admite SMB versión 2.0 o 3.0.
  3. Puedes forzar un re-escaneo desde la aplicación tocando en **Ajustes > Re-escanear Biblioteca**.

