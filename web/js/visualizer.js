// =====================================================================
// AuraBox - Real-Time Audio Frequency Spectrum Visualizer (32 Bands)
// Native Web Audio API (AudioContext + AnalyserNode + FFT 512)
// Logarithmic Frequency Band Distribution & Peak Falloff Smoothing
// =====================================================================

import { state, subscribe } from './state.js';

let audioCtx = null;
let analyserNode = null;
let gainNode = null;
let mediaSourceNode = null;
let isAudioContextInitialized = false;

let cachedBars = [];
let lastBarScales = new Int16Array(32).fill(-1);
let animFrameId = null;
let lastFrameTimeMs = 0;
let freqDataBuffer = null;
let currentHeights = new Float32Array(32).fill(0.08);
const VISUALIZER_FRAME_INTERVAL_MS = 50; // ~20 FPS: suave a la vista y ligero para CPU/GPU en Raspberry Pi

let isAudible = false;
let isVisualizerStreaming = false;

// TOTAL_BARS define el número de barras de frecuencia espectral en la interfaz.
const TOTAL_BARS = 32;

// RESTING_HEIGHT define la altura base mínima elegante de reposo (2px).
const RESTING_HEIGHT = 0.08;

// TILT_FACTORS define la curva de compensación acústica para equilibrar graves físicos con agudos audibles.
const TILT_FACTORS = (() => {
  const arr = new Float32Array(TOTAL_BARS);
  for (let i = 0; i < TOTAL_BARS; i++) {
    // Progresión suave que realza el espectro audible medio y alto (voz, guitarras, platillos)
    arr[i] = 1.0 + Math.pow(i / (TOTAL_BARS - 1), 1.25) * 1.4;
  }
  return arr;
})();

// LOG_BIN_RANGES calcula la partición logarítmica de los 256 bins FFT en las 32 barras audiófilas.
const LOG_BIN_RANGES = (() => {
  const ranges = [];
  const binCount = 256;
  const minBin = 1;
  const maxBin = binCount - 12;

  for (let i = 0; i < TOTAL_BARS; i++) {
    const startRatio = Math.pow(i / TOTAL_BARS, 2.2);
    const endRatio = Math.pow((i + 1) / TOTAL_BARS, 2.2);

    const startBin = Math.max(0, Math.floor(minBin + startRatio * (maxBin - minBin)));
    const endBin = Math.min(binCount - 1, Math.max(startBin + 1, Math.floor(minBin + endRatio * (maxBin - minBin))));
    ranges.push({ start: startBin, end: endBin });
  }
  return ranges;
})();

// setupAudioGraph conecta el elemento HTML5 Audio al grafo de Web Audio API con AnalyserNode y GainNode.
function setupAudioGraph(audioElement) {
  if (mediaSourceNode || !audioElement) return;

  try {
    const AudioContextClass = window.AudioContext || window.webkitAudioContext;
    if (!AudioContextClass) return;

    if (!audioCtx) {
      audioCtx = new AudioContextClass();
    }

    analyserNode = audioCtx.createAnalyser();
    analyserNode.fftSize = 512; // 256 bins de frecuencia (resolución fina de 86 Hz por bin)
    analyserNode.smoothingTimeConstant = 0.82; // Inercia analógica para caída fluida de picos
    analyserNode.minDecibels = -90;
    analyserNode.maxDecibels = -10;

    gainNode = audioCtx.createGain();
    gainNode.gain.value = isAudible ? 1.0 : 0.0;

    mediaSourceNode = audioCtx.createMediaElementSource(audioElement);
    mediaSourceNode.connect(analyserNode);
    analyserNode.connect(gainNode);
    gainNode.connect(audioCtx.destination);

    isAudioContextInitialized = true;
  } catch (err) {
    console.warn('[Visualizer] Web Audio API no pudo enlazarse directamente (se aplicará síntesis rítmica sincronizada):', err);
  }
}

// ensureAudioContextRunning despierta el AudioContext en interacción del usuario para satisfacer políticas de navegadores.
export function ensureAudioContextRunning() {
  if (!audioCtx) {
    const audioElement = document.getElementById('webAudioPlayer');
    if (audioElement) {
      setupAudioGraph(audioElement);
    }
  }
  if (audioCtx && audioCtx.state === 'suspended') {
    audioCtx.resume().catch(() => {});
  }
}

// setVisualizerAudible activa o silencia la salida de altavoz del stream en el dispositivo cliente.
export function setVisualizerAudible(audible) {
  isAudible = Boolean(audible);
  if (gainNode && audioCtx) {
    try {
      const targetGain = isAudible ? 1.0 : 0.0;
      gainNode.gain.setTargetAtTime(targetGain, audioCtx.currentTime, 0.05);
    } catch (_) {
      gainNode.gain.value = isAudible ? 1.0 : 0.0;
    }
  }
}

// prefersReducedMotion comprueba si el sistema solicita animaciones reducidas para ahorrar recursos.
function prefersReducedMotion() {
  return typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

// startVisualizerLoop arranca el ciclo de animación regulado si no está activo y la vista está abierta.
function startVisualizerLoop() {
  if (animFrameId !== null) return;
  if (prefersReducedMotion()) return;
  animFrameId = requestAnimationFrame(renderFrame);
}

// stopVisualizerLoop cancela la animación y devuelve las 32 barras a su altura mínima de reposo.
function stopVisualizerLoop() {
  if (animFrameId !== null) {
    cancelAnimationFrame(animFrameId);
    animFrameId = null;
  }
  for (let i = 0; i < TOTAL_BARS; i++) {
    currentHeights[i] = RESTING_HEIGHT;
    const restingScale = Math.round(RESTING_HEIGHT * 1000);
    if (cachedBars[i] && lastBarScales[i] !== restingScale) {
      lastBarScales[i] = restingScale;
      cachedBars[i].style.transform = `scaleY(${RESTING_HEIGHT})`;
    }
  }
}

// renderFrame lee magnitudes FFT reales (si el audio web está activo) o aplica síntesis ligera regulada a ~20 FPS.
function renderFrame(timestamp) {
  animFrameId = null;

  const isPlaying = (state.player.status === 'play');
  const isModalOpen = Boolean(state.ui.isNowPlayingOpen);
  const container = document.getElementById('npSpectrumVisualizer');

  if (!container || !isModalOpen || document.visibilityState === 'hidden') {
    stopVisualizerLoop();
    return;
  }

  const nowMs = timestamp || performance.now();
  if (nowMs - lastFrameTimeMs < VISUALIZER_FRAME_INTERVAL_MS) {
    animFrameId = requestAnimationFrame(renderFrame);
    return;
  }
  lastFrameTimeMs = nowMs;

  let hasRealSignal = false;
  let freqData = null;

  if (isAudible && analyserNode && isAudioContextInitialized) {
    if (!freqDataBuffer) {
      freqDataBuffer = new Uint8Array(analyserNode.frequencyBinCount);
    }
    analyserNode.getByteFrequencyData(freqDataBuffer);

    // Comprobar si hay energía acústica real en el buffer FFT
    for (let k = 0; k < freqDataBuffer.length; k += 4) {
      if (freqDataBuffer[k] > 6) {
        hasRealSignal = true;
        break;
      }
    }
    if (hasRealSignal) {
      freqData = freqDataBuffer;
    }
  }

  const nowSec = nowMs / 1000;
  let allResting = true;

  for (let i = 0; i < TOTAL_BARS; i++) {
    let target = RESTING_HEIGHT;

    if (isPlaying) {
      if (hasRealSignal && freqData) {
        // 1. Cálculo logarítmico real desde los bins FFT
        const r = LOG_BIN_RANGES[i];
        let sum = 0;
        let count = 0;
        for (let b = r.start; b < r.end; b++) {
          sum += freqData[b];
          count++;
        }
        const avg = count > 0 ? (sum / count) : freqData[r.start];
        const normalized = (avg / 255.0) * TILT_FACTORS[i];
        target = Math.max(RESTING_HEIGHT, Math.min(1.0, normalized));
      } else {
        // 2. Síntesis rítmica orgánica sin streaming oculto para no cargar CPU de Raspberry Pi
        const bassWave = Math.sin(nowSec * 5.2 + i * 0.18) * 0.5 + 0.5;
        const midWave = Math.cos(nowSec * 7.8 - i * 0.32) * 0.5 + 0.5;
        const kickPulse = (i < 8) ? Math.max(0, Math.sin(nowSec * 8.6)) * 0.48 : 0;
        const hiPulse = (i >= 24) ? Math.max(0, Math.sin(nowSec * 16.0 + i)) * 0.35 : 0;
        const synth = (bassWave * 0.3 + midWave * 0.3 + kickPulse + hiPulse) * (1.0 - i / 48);
        target = Math.max(RESTING_HEIGHT, Math.min(0.92, 0.12 + synth));
      }
    }

    // 3. Inercia de caída analógica (compensada para 20 FPS)
    if (target >= currentHeights[i]) {
      currentHeights[i] = target;
    } else {
      currentHeights[i] = Math.max(RESTING_HEIGHT, currentHeights[i] * 0.72);
    }

    if (Math.abs(currentHeights[i] - RESTING_HEIGHT) > 0.005) {
      allResting = false;
    }

    const quantized = Math.round(currentHeights[i] * 100);
    if (cachedBars[i] && lastBarScales[i] !== quantized) {
      lastBarScales[i] = quantized;
      cachedBars[i].style.transform = `scaleY(${(quantized / 100).toFixed(2)})`;
    }
  }

  // Continuar el bucle mientras esté sonando o mientras las barras sigan cayendo al reposo
  if (isPlaying || !allResting) {
    animFrameId = requestAnimationFrame(renderFrame);
  }
}

// syncVisualizerState sincroniza animación y audio web evitando abrir /api/stream en segundo plano si no es audible.
export function syncVisualizerState() {
  const isPlaying = (state.player.status === 'play');
  const isModalOpen = Boolean(state.ui.isNowPlayingOpen);
  const audioElement = document.getElementById('webAudioPlayer');

  // Nunca abrir ni mantener un stream MP3 silencioso en /api/stream cuando el audio suena por la salida física de la RPi
  if (!isAudible && isVisualizerStreaming && audioElement) {
    audioElement.pause();
    audioElement.removeAttribute('src');
    audioElement.load();
    isVisualizerStreaming = false;
  }

  if (isPlaying && isModalOpen && document.visibilityState !== 'hidden') {
    if (isAudible) {
      ensureAudioContextRunning();
      if (audioElement && (!audioElement.src || audioElement.paused)) {
        if (!audioElement.hasAttribute('src') || !audioElement.src.includes('/api/stream')) {
          audioElement.src = `/api/stream?_t=${Date.now()}`;
          audioElement.load();
        }
        const p = audioElement.play();
        if (p !== undefined) {
          p.catch(() => {});
        }
        isVisualizerStreaming = true;
      }
    }
    startVisualizerLoop();
  } else {
    if (!isPlaying && audioElement && !isAudible && !audioElement.paused) {
      audioElement.pause();
      isVisualizerStreaming = false;
    }
    if (!isPlaying && isModalOpen) {
      // Permitir decaimiento suave a 0 de las barras solo si el modal sigue abierto
      startVisualizerLoop();
    } else if (!isModalOpen) {
      stopVisualizerLoop();
    }
  }
}

// initVisualizer inicializa escuchadores de eventos, referencias DOM y desbloqueo táctil del motor de audio.
export function initVisualizer() {
  cachedBars = Array.from(document.querySelectorAll('#npSpectrumVisualizer .spec-bar'));

  // Desbloqueo pasivo en primera interacción táctil o de teclado
  const unlockEvents = ['pointerdown', 'touchstart', 'click', 'keydown'];
  const handleUserGesture = () => {
    ensureAudioContextRunning();
    unlockEvents.forEach((evt) => window.removeEventListener(evt, handleUserGesture));
  };
  unlockEvents.forEach((evt) => window.addEventListener(evt, handleUserGesture, { capture: true, passive: true }));

  // Suscribirse a cambios reactivos de Now Playing y reproducción
  subscribe((event) => {
    if (event === 'player_updated' || event === 'now_playing_toggled') {
      syncVisualizerState();
    }
  });

  // Pausar procesamiento si la pestaña no es visible
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      syncVisualizerState();
    } else {
      if (animFrameId !== null) {
        cancelAnimationFrame(animFrameId);
        animFrameId = null;
      }
    }
  });

  syncVisualizerState();
}
