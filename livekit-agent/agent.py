import asyncio
import logging
import json
import os
import signal
import time
import aiohttp
from aiohttp import web
from livekit import api, rtc
from livekit.plugins import deepgram, google
from dotenv import load_dotenv
from opentelemetry import _logs, trace
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.instrumentation.logging import LoggingInstrumentor
from opentelemetry.instrumentation.aiohttp_server import AioHttpServerInstrumentor
from opentelemetry.instrumentation.aiohttp_client import AioHttpClientInstrumentor

load_dotenv()

# Configuration
LOG_LEVEL = logging.INFO
MAX_CONCURRENT_ROOMS = 100 # Optimized for IO-bound workload
AGENT_IDENTITY = "transcriber-bot"

# Runtime transcription config fetched from the backend (admin-managed),
# cached briefly so we don't hit the BE on every track/utterance. Falls back
# to env vars when the BE is unreachable or hasn't shipped the endpoint.
_CONFIG_CACHE = {"value": None, "fetched_at": 0.0}
_CONFIG_TTL_S = 30.0


def _config_base_url():
    """Derive the BE base URL from BACKEND_URL (which points at /livekit/transcript)."""
    backend = os.getenv("BACKEND_URL", "")
    if not backend:
        return ""
    # Strip the known transcript suffix to get the /livekit base.
    if backend.endswith("/transcript"):
        return backend[: -len("/transcript")]
    return backend


async def fetch_transcription_config(http_session):
    """Return the admin-managed transcription config dict, cached for _CONFIG_TTL_S.

    Shape: {mode, stt_provider, stt_model, stt_base_url, stt_language,
    stt_api_key, google_credentials}.
    On any failure returns None and callers
    fall back to environment variables — so a BE blip never stops transcription.
    """
    now = time.time()
    cached = _CONFIG_CACHE["value"]
    if cached is not None and (now - _CONFIG_CACHE["fetched_at"]) < _CONFIG_TTL_S:
        return cached

    base = _config_base_url()
    if not base:
        return None
    url = f"{base}/transcription-config"
    secret = os.getenv("INTERNAL_SECRET", "")
    headers = {"X-Internal-Secret": secret} if secret else {}
    try:
        async with http_session.get(url, headers=headers, timeout=aiohttp.ClientTimeout(total=5)) as resp:
            if resp.status != 200:
                logger.warning(f"transcription-config fetch failed: {resp.status}")
                return cached  # keep last-known-good if we have one
            data = await resp.json()
            _CONFIG_CACHE["value"] = data
            _CONFIG_CACHE["fetched_at"] = now
            return data
    except Exception as e:
        logger.warning(f"transcription-config fetch error: {e}")
        return cached


# Tracks the temp file we wrote the Google service-account JSON to, so we only
# rewrite it when the credential content changes.
_GOOGLE_CREDS_STATE = {"written_hash": None, "path": "/tmp/onecamp-google-creds.json"}


def _ensure_google_credentials_file(creds_json):
    """Write the admin-provided Google credentials JSON to a temp file and point
    GOOGLE_APPLICATION_CREDENTIALS at it. Idempotent: only rewrites on change."""
    import hashlib
    h = hashlib.sha256(creds_json.encode("utf-8")).hexdigest()
    if _GOOGLE_CREDS_STATE["written_hash"] == h:
        return
    try:
        path = _GOOGLE_CREDS_STATE["path"]
        with open(path, "w") as f:
            f.write(creds_json)
        os.environ["GOOGLE_APPLICATION_CREDENTIALS"] = path
        _GOOGLE_CREDS_STATE["written_hash"] = h
    except Exception as e:
        logger.error(f"Failed to write Google credentials file: {e}")


_VAD_SINGLETON = None


def _shared_vad():
    """Load Silero VAD once per process.

    Loading it is expensive and it holds no per-call state, so a singleton is
    both cheaper and correct. Lazy, because the streaming providers never need
    it and an import cost at module load would be paid by every deployment.
    """
    global _VAD_SINGLETON
    if _VAD_SINGLETON is None:
        from livekit.plugins import silero  # lazy: only needed for non-streaming STT

        _VAD_SINGLETON = silero.VAD.load()
    return _VAD_SINGLETON


def _ensure_streaming(stt_inst):
    """Return an STT that supports .stream(), adapting one that does not.

    WHY THIS EXISTS. Whisper has no streaming API. Any STT built on the plain
    /v1/audio/transcriptions endpoint (OpenAI, Groq, and every self-hosted
    Whisper server) can only transcribe a finished clip, so calling .stream() on
    it raises NotImplementedError: "streaming is not supported by this STT,
    please use a different STT or use a StreamAdapter". The transcriber calls
    .stream() on whatever it is given, so the OpenAI-compatible provider this
    admin UI has always offered could never actually work. It failed inside a
    per-participant task, so the call simply had no captions, no transcript and
    therefore no recap, with one line in a container log.

    StreamAdapter is the supported fix: VAD decides where an utterance ends and
    hands that clip to the non-streaming model.

    Keyed on the STT's own declared capability rather than on the provider name,
    so a plugin that gains native streaming stops being adapted without anyone
    editing this, and a new non-streaming provider is handled the day it is
    added.
    """
    caps = getattr(stt_inst, "capabilities", None)
    if caps is None or getattr(caps, "streaming", True):
        return stt_inst

    from livekit.agents import stt as agent_stt  # lazy, same reason as the VAD

    logger.info("STT does not support streaming; wrapping it with VAD segmentation")
    return agent_stt.StreamAdapter(stt=stt_inst, vad=_shared_vad())


# How much silence to append when a track ends, in 20ms frames. Comfortably
# past the pause any VAD treats as end-of-utterance, and it costs nothing: it is
# appended once per track, after the speaker has already gone.
_FLUSH_SILENCE_FRAMES = 75


def _flush_trailing_utterance(stt_stream, last_frame):
    """Close an utterance that was still open when the track ended.

    A VAD-segmented STT only sends a clip to the model when it decides the
    utterance is OVER, and it decides that from a pause. When a track stops
    mid-sentence, which is what happens when somebody hangs up while talking or
    leaves the call, there is no pause: the audio simply stops, end_input()
    closes the stream, and everything said since the last pause is discarded.

    Measured, not assumed. Pushing a clip with no trailing silence produces
    START_OF_SPEECH and then nothing at all; the same clip with two seconds of
    silence appended produces END_OF_SPEECH and the transcript.

    So the silence a real pause would have provided is appended here. Only
    matters for the adapted path, and it is harmless on a native streaming STT,
    which has already sent everything it heard.
    """
    if last_frame is None:
        return
    try:
        samples = getattr(last_frame, "samples_per_channel", 0) or 0
        channels = getattr(last_frame, "num_channels", 1) or 1
        rate = getattr(last_frame, "sample_rate", 0) or 0
        if samples <= 0 or rate <= 0:
            return
        quiet = b"\x00" * (samples * channels * 2)  # 16-bit PCM
        for _ in range(_FLUSH_SILENCE_FRAMES):
            stt_stream.push_frame(rtc.AudioFrame(
                data=quiet,
                sample_rate=rate,
                num_channels=channels,
                samples_per_channel=samples,
            ))
    except Exception as e:
        # Never let the flush be the reason a track teardown fails; the worst
        # case without it is one lost trailing utterance.
        logger.warning(f"Could not flush the trailing utterance: {e}")


def _build_stt(provider, model, base_url, language):
    """Construct a LiveKit STT plugin instance from admin-managed config.

    Model-agnostic by design: the provider KIND selects the plugin, and the
    remaining fields (model / base_url / language) are passed through when set.
    Adding a new STT is a config change here, not a schema change:
      - deepgram → livekit-plugins-deepgram
      - google   → livekit-plugins-google
      - openai   → livekit-plugins-openai, which also covers ANY OpenAI-
                   compatible STT (OpenAI Whisper, Groq, self-hosted
                   faster-whisper / vLLM) via base_url. This is the path a
                   future self-hosted Whisper deployment plugs into.
    Returns None for an unknown provider so the caller can skip gracefully.
    """
    p = (provider or "deepgram").lower()
    try:
        if p == "google":
            kwargs = {}
            if language:
                kwargs["languages"] = [language]
            return google.STT(**kwargs)
        if p == "openai":
            from livekit.plugins import openai as _openai  # lazy: optional plugin
            kwargs = {}
            if model:
                kwargs["model"] = model
            if base_url:
                kwargs["base_url"] = base_url
            if language:
                kwargs["language"] = language
            return _openai.STT(**kwargs)
        # default: deepgram
        kwargs = {"model": model or "nova-2"}
        if language:
            kwargs["language"] = language
        return deepgram.STT(**kwargs)
    except Exception as e:
        logger.error(f"Failed to build STT for provider '{p}': {e}")
        return None

def _otlp_endpoint(raw):
    """Return an absolute OTLP/HTTP base URL, or "" if none is configured.

    OTEL_EXPORTER_OTLP_ENDPOINT is conventionally written host:port
    (otel-collector:4318), but the OTLP/HTTP exporters hand the value to
    requests, which rejects a schemeless URL with InvalidSchema. Every export
    therefore failed on every batch, so no log or span ever left the agent.
    """
    raw = (raw or "").strip().rstrip("/")
    if not raw:
        return ""
    return raw if "://" in raw else "http://" + raw


def setup_otel():
    endpoint = _otlp_endpoint(os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
    if not endpoint:
        return

    # Create OTLP HTTP exporter for logs
    # Note: endpoint usually expects the full path for logs inproto-http
    # but some collectors handle the base. HyperDX/Otel Collector usually needs /v1/logs
    # However, OTLPLogExporter (http) by default appends /v1/logs if not present in some versions.
    # In many versions, it's safer to provide the exact endpoint or check if it's appended.
    
    # Matching Go backend pattern: it uses endpoint as base.
    log_exporter = OTLPLogExporter(
        endpoint=f"{endpoint}/v1/logs",
        headers={"authorization": os.getenv("HYPERDX_INGESTION_API_KEY", "")}
    )

    resource = Resource.create({
        "service.name": os.getenv("OTEL_SERVICE_NAME", "transcription-agent"),
        "environment": "production"
    })

    logger_provider = LoggerProvider(resource=resource)
    _logs.set_logger_provider(logger_provider)
    logger_provider.add_log_record_processor(BatchLogRecordProcessor(log_exporter))

    # Tracing
    span_exporter = OTLPSpanExporter(
        endpoint=f"{endpoint}/v1/traces",
        headers={"authorization": os.getenv("HYPERDX_INGESTION_API_KEY", "")}
    )
    tracer_provider = TracerProvider(resource=resource)
    trace.set_tracer_provider(tracer_provider)
    tracer_provider.add_span_processor(BatchSpanProcessor(span_exporter))

    # Instrument standard logging to capture all logs
    handler = LoggingHandler(level=logging.ERROR, logger_provider=logger_provider)
    logging.getLogger().addHandler(handler)
    LoggingInstrumentor().instrument(set_logging_format=True)

    # Instrument aiohttp server and client
    AioHttpServerInstrumentor().instrument()
    AioHttpClientInstrumentor().instrument()

# basicConfig BEFORE setup_otel, and this order is load-bearing. basicConfig is
# a no-op when the root logger already has a handler, and setup_otel attaches one
# (an OTEL LoggingHandler at ERROR). With the calls the other way round the agent
# got no stream handler at all: nothing on stdout, `docker logs` empty for days,
# and every logger.warning discarded — including the one saying the runtime
# transcription-config fetch was failing. A transcription agent that cannot say
# why it is not transcribing is the hardest kind of outage to find.
logging.basicConfig(level=LOG_LEVEL, format='%(asctime)s - %(name)s - %(levelname)s - %(message)s')

setup_otel()

logger = logging.getLogger("transcription-agent")

class TranscriptionService:
    def __init__(self):
        self.running_rooms = {} 
        self.shutdown_event = asyncio.Event()
        # LiveKitAPI initialized in start() to share session
        self.lkapi = None
        self.webhook_receiver = api.WebhookReceiver(
            api.TokenVerifier(
                os.getenv("LIVEKIT_API_KEY"),
                os.getenv("LIVEKIT_API_SECRET")
            )
        )
        
    async def start(self):
        # Initialize Shared Session with Connection Pooling
        # optimize: limit pool size to avoid fd exhaustion, but allow high concurrency
        connector = aiohttp.TCPConnector(limit=100, limit_per_host=100)
        async with aiohttp.ClientSession(connector=connector) as session:
            self.http_session = session
            # Initialize LiveKitAPI with shared session
            self.lkapi = api.LiveKitAPI(
                os.getenv("LIVEKIT_URL"),
                os.getenv("LIVEKIT_API_KEY"),
                os.getenv("LIVEKIT_API_SECRET"),
            )

            # Start HTTP Server
            app = web.Application()
            app.add_routes([
                web.get('/health', self.handle_health),
                web.post('/webhook', self.handle_webhook)
            ])
            
            runner = web.AppRunner(app)
            await runner.setup()
            site = web.TCPSite(runner, '0.0.0.0', 8080)
            await site.start()
            logger.info("HTTP Server listening on 8080 (Webhooks Enabled)")
            
            # Initial Scan for existing rooms (in case agent restarted mid-session)
            asyncio.create_task(self.check_existing_rooms())
            
            try:
                # Wait for shutdown
                await self.shutdown_event.wait()
            finally:
                # Cleanup
                logger.info("Shutting down...")
                await site.stop()
                for room_mgr in self.running_rooms.values():
                    await room_mgr.stop()
                    
                # No need to close HTTP session explicitly, context manager handles it
                # But we should close LKAPI if it has internal resources other than session
                # (Standard practice is harmless)
                await self.lkapi.aclose() 
                pass

    async def check_existing_rooms(self):
        try:
            logger.info("Checking for existing rooms on startup...")
            rooms = await self.lkapi.room.list_rooms(api.ListRoomsRequest())
            if not rooms.rooms:
                logger.info("No existing rooms found.")
                return
                
            for room in rooms.rooms:
                if room.num_participants > 0:
                    logger.info(f"Found existing active room: {room.name}")
                    await self.join_room(room.name)
                else:
                    logger.info(f"Skipping empty room: {room.name}")
        except Exception as e:
            logger.error(f"Failed to check existing rooms: {e}")

    async def handle_webhook(self, request):
        try:
            # Log raw hit (DEBUG only)
            logger.debug(f"Webhook Endpoint Hit! Method: {request.method}, Path: {request.path}")
            
            auth_header = request.headers.get("Authorization")
            if not auth_header:
                logger.warning("Webhook missing Authorization header")
                return web.Response(text="Missing Authorization Header", status=401)
                
            body = await request.text()
            logger.debug(f"Webhook Body ({len(body)} bytes): {body[:200]}...") 
            
            try:
                event = self.webhook_receiver.receive(body, auth_header)
            except Exception as auth_err:
                logger.error(f"Webhook Verification Failed: {auth_err}")
                return web.Response(text="Webhook Verification Failed", status=401)
            
            logger.info(f"Webhook Verified. Event: {event.event}, Room: {event.room.name if event.room else 'None'}")
            
            should_join = False
            if event.event == "room_started":
                should_join = True
            elif event.event == "participant_joined":
                if event.participant.identity != AGENT_IDENTITY:
                    should_join = True
            
            if should_join:
                 await self.join_room(event.room.name)

            if event.event == "room_finished":
                self.cleanup_room(event.room.name)
                
            return web.Response(text="OK", status=200)
        except Exception as e:
            logger.error(f"Webhook error: {e}")
            return web.Response(text=str(e), status=500)

    async def join_room(self, room_name):
        if room_name in self.running_rooms:
            logger.info(f"Already in room {room_name}")
            return
            
        if len(self.running_rooms) >= MAX_CONCURRENT_ROOMS:
            logger.warning(f"Max rooms reached, ignoring {room_name}")
            return

        logger.info(f"Joining room: {room_name}")
        room_mgr = RoomManager(room_name, self.http_session, self.cleanup_room)
        self.running_rooms[room_name] = room_mgr
        asyncio.create_task(room_mgr.start())

    def cleanup_room(self, room_name):
        if room_name in self.running_rooms:
            # We don't await stop here to avoid blocking webhook handler, 
            # assuming RoomManager handles its own cleanup or we trigger it async if needed.
            # But usually RoomManager.start() loop exits on disconnect.
            # If explicit stop needed:
            room_task = self.running_rooms[room_name]
            # Since we store the instance, we can call stop signal? 
            # ideally RoomManager should observe connection state.
            # But let's trigger explicit shutdown to be safe.
            asyncio.create_task(room_task.stop())
            del self.running_rooms[room_name]
            logger.info(f"Cleaned up room {room_name}")

    async def handle_health(self, request):
        return web.Response(text="OK", status=200)

class RoomManager:
    def __init__(self, room_name, http_session, cleanup_cb):
        self.room_name = room_name
        self.http_session = http_session
        self.cleanup_cb = cleanup_cb
        self.room = rtc.Room()
        self.shutdown_flag = asyncio.Event()
        # Agent-clock wall time (seconds) at which this manager first observed
        # the active recording (egress) in room metadata, plus the egress id it
        # belongs to. Backend-mode offsets are measured from this anchor in the
        # agent's own clock, so the recorded-video seek position is free of the
        # agent-vs-egress-server clock skew. None until a recording is seen.
        self._recording_start_s = None
        self._recording_egress = ""

    def _recording_anchor_s(self):
        """Return (start_seconds, egress_id), refreshing from room metadata.

        Stamps the agent clock the first time a NEW egress id appears and
        clears the anchor when recording stops, so each recording re-anchors
        cleanly. Returns (None, "") when no recording is active.
        """
        egress_id = ""
        is_recording = False
        try:
            if self.room.metadata:
                meta = json.loads(self.room.metadata)
                is_recording = bool(meta.get("isRecording"))
                egress_id = meta.get("egressID", "")
        except Exception:
            pass
        if is_recording and egress_id:
            if self._recording_egress != egress_id:
                self._recording_egress = egress_id
                self._recording_start_s = time.time()
            return self._recording_start_s, egress_id
        self._recording_egress = ""
        self._recording_start_s = None
        return None, ""

    async def start(self):
        try:
            # Token generation
            token = api.AccessToken(
                os.getenv("LIVEKIT_API_KEY"),
                os.getenv("LIVEKIT_API_SECRET")
            ).with_identity(AGENT_IDENTITY).with_name("Transcriber Bot").with_grants(api.VideoGrants(
                room_join=True,
                room=self.room_name,
                can_publish=True,
                can_subscribe=True,
            )).to_jwt()

            # Event handlers
            @self.room.on("track_subscribed")
            def on_track_subscribed(track, publication, participant):
                if track.kind == rtc.TrackKind.KIND_AUDIO:
                    # Mode is resolved at runtime inside _handle_audio_track
                    # (admin config, env fallback). We always spawn the handler
                    # and let it decide whether to run backend STT, so a mode
                    # change mid-session is honored on the next subscribed track.
                    logger.info(f"Subscribed to audio track from {participant.identity}")
                    asyncio.create_task(self._handle_audio_track(track, participant))

            @self.room.on("data_received")
            def on_data_received(event):
                # LiveKit Python SDK passes a DataPacketReceivedEvent object
                data = event.data
                participant = event.participant
                topic = event.topic
                
                logger.debug(f"Data received: topic={topic}, len={len(data)}")
                
                if topic == "lk.transcription":
                    try:
                        payload = json.loads(data.decode('utf-8'))
                        # Support both backend and frontend generated transcripts
                        # Frontend sends 'isFinal', Backend logic below sends 'isFinal' too.
                        is_final = payload.get("isFinal")
                        text = payload.get("text", "")
                        
                        # Avoid double saving if we (the agent) sent it?
                        # If agent sends it, participant is None or Agent?
                        # When agent publishes, it comes from Agent participant.
                        # But we want to save it associated with the ORIGINAL speaker.
                        # The payload should contain original speaker info if possible, or we rely on 'participant' arg.
                        
                        # If generic agent published it, 'participant' is the agent.
                        # We need to extract real speaker identity from payload if possible.
                        real_participant_identity = payload.get("participantIdentity")
                        
                        if is_final and text:
                             # Use payload identity if available (from backend logic), else sender (frontend logic)
                             identity_to_log = real_participant_identity if real_participant_identity else (participant.identity if participant else "Unknown")
                             
                             logger.debug(f"TRANSCRIPT [{identity_to_log}]: {text}")
                             
                             # Extract timestamp or duration
                             # Ideally use DURATION to rebase to Server Clock (avoids client clock skew)
                             ts_val = payload.get("timestamp")
                             duration_val = payload.get("duration")
                             
                             ts_ms_val = None
                             
                             if duration_val is not None:
                                 # Rebase to Server Clock: ServerNow - Duration
                                 # time.time() is UTC.
                                 server_now_ms = int(time.time() * 1000)
                                 ts_ms_val = int(server_now_ms - float(duration_val))
                                 logger.debug(f"Rebased Timestamp (Server Clock): {ts_ms_val} (Duration: {duration_val}ms)")
                             elif ts_val:
                                 # Fallback to Client Clock (might be skewed)
                                 ts_ms_val = int(ts_val)
                                 logger.debug(f"Client Timestamp (Potential Skew): {ts_ms_val}")
                                 
                             # Pass Milliseconds directly (int64)
                             # Forward the producer's skew-free offset (ms from
                             # recording start, browser clock) when present so
                             # playback seeks accurately regardless of clock skew.
                             offset_val = payload.get("offsetMs")
                             offset_ms = int(offset_val) if offset_val is not None else None
                             asyncio.create_task(self.save_to_backend(identity_to_log, text, timestamp=ts_ms_val, offset_ms=offset_ms))
                    except Exception as e:
                        logger.error(f"Error handling transcript data: {e}")

            await self.room.connect(os.getenv("LIVEKIT_URL"), token)
            logger.info(f"Connected to {self.room_name}")

            # Monitor loop
            while self.room.connection_state == rtc.ConnectionState.CONN_CONNECTED and not self.shutdown_flag.is_set():
                 # Auto-leave if empty
                if len(self.room.remote_participants) == 0:
                     # Wait a bit to be sure
                     await asyncio.sleep(10)
                     if len(self.room.remote_participants) == 0:
                         logger.info(f"Room {self.room_name} empty. Leaving.")
                         break
                await asyncio.sleep(5)

        except Exception as e:
            logger.error(f"Room {self.room_name} error: {e}")
        finally:
            await self.stop()

    async def _handle_audio_track(self, track: rtc.Track, participant: rtc.RemoteParticipant):
        # Resolve runtime config (admin-managed, env fallback). Only run
        # server-side STT in backend mode; in frontend/off mode the browser
        # path (or nothing) handles transcription and we must not double-emit.
        cfg = await fetch_transcription_config(self.http_session)
        mode = (cfg or {}).get("mode") or os.getenv("TRANSCRIPTION_MODE", "backend")
        if mode != "backend":
            logger.info(f"Mode '{mode}' — skipping backend STT for {participant.identity}")
            return

        stt_provider = (cfg or {}).get("stt_provider") or os.getenv("STT_PROVIDER", "deepgram")
        stt_model = (cfg or {}).get("stt_model") or os.getenv("STT_MODEL", "")
        stt_base_url = (cfg or {}).get("stt_base_url") or os.getenv("STT_BASE_URL", "")
        stt_language = (cfg or {}).get("stt_language") or os.getenv("STT_LANGUAGE", "")
        # Inject admin-managed secrets into the process env the LiveKit plugins
        # read, when provided by the BE. Env stays the fallback for callers that
        # still configure keys the old way.
        if cfg:
            if cfg.get("stt_api_key"):
                # The deepgram and openai plugins both read their key from env.
                # Set both names so the chosen plugin picks it up regardless.
                os.environ["DEEPGRAM_API_KEY"] = cfg["stt_api_key"]
                os.environ["OPENAI_API_KEY"] = cfg["stt_api_key"]
            if cfg.get("google_credentials"):
                # The google plugin reads GOOGLE_APPLICATION_CREDENTIALS as a
                # FILE path; write the JSON to a temp file once and point at it.
                _ensure_google_credentials_file(cfg["google_credentials"])

        logger.info(f"Starting transcription for {participant.identity} using {stt_provider} (model={stt_model or 'default'})")

        stt_stream = None
        audio_stream = rtc.AudioStream(track)
        stream_start_time = time.time()

        try:
            stt_inst = _build_stt(stt_provider, stt_model, stt_base_url, stt_language)
            if stt_inst is None:
                logger.error(f"Unsupported STT provider '{stt_provider}'; skipping transcription")
                return

            stt_stream = _ensure_streaming(stt_inst).stream()
            
            async def push_audio():
                last_frame = None
                async for frame in audio_stream:
                    if self.shutdown_flag.is_set(): break
                    last_frame = frame
                    stt_stream.push_frame(frame)
                _flush_trailing_utterance(stt_stream, last_frame)
                stt_stream.end_input()
                
            async def receive_results():
                async for event in stt_stream:
                    if self.shutdown_flag.is_set(): break
                    
                    if event.type == api.SpeechEventType.FINAL_TRANSCRIPT_EVENT:
                        alt = event.alternatives[0]
                        text = alt.text
                        if not text: continue
                        
                        logger.debug(f"STT Result ({participant.identity}): {text}")
                        
                        # Timestamp Logic:
                        # Default to current time (End of utterance)
                        now_ms = int(time.time() * 1000)
                        final_timestamp_ms = now_ms
                        
                        # Try to find start time from word timings (Deepgram/Google usually provide this)
                        # Structure varies by plugin, but 'words' list is common standard in LiveKit STT
                        start_offset_s = 0.0
                        duration_s = 0.0
                        
                        # Heuristic fallback duration (0.4s per word)
                        word_count = len(text.split())
                        heuristic_duration_s = max(1.0, word_count * 0.4)

                        found_timing = False
                        
                        # Check for words/timing
                        # We use getattr to be safe against different plugin versions
                        words = getattr(alt, 'words', [])
                        if words and len(words) > 0:
                            first_word = words[0]
                            # start_time is usually in seconds relative to stream start
                            start_s = getattr(first_word, 'start_time', 0.0)
                            end_s = getattr(words[-1], 'end_time', 0.0)
                            
                            start_offset_s = start_s
                            duration_s = end_s - start_s
                            found_timing = True
                            
                            # Calculate absolute start time
                            # stream_start_time is wall clock when we started stream
                            # final_timestamp_ms should be the START of the utterance for correct seeking
                            final_timestamp_ms = int((stream_start_time + start_offset_s) * 1000)
                            logger.debug(f"Timing Found: offset={start_offset_s:.2f}s, abs_ts={final_timestamp_ms}")

                        else:
                            # Fallback: Subtract heuristic duration from NOW
                            # Logic: We received it just now, so it ended just now. Started 'duration' ago.
                            final_timestamp_ms = int((time.time() - heuristic_duration_s) * 1000)
                            logger.debug(f"Timing Fallback: duration={heuristic_duration_s:.2f}s, abs_ts={final_timestamp_ms}")

                        payload = {
                            "participantIdentity": participant.identity,
                            "participantName": participant.name or participant.identity,
                            "text": text,
                            "timestamp": final_timestamp_ms, 
                            "duration": duration_s if found_timing else heuristic_duration_s, # Pass duration if helpful
                            "isFinal": True,
                            "id": f"backend-{participant.identity}-{final_timestamp_ms}",
                            "source": "backend"
                        }
                        
                        data = json.dumps(payload).encode('utf-8')
                        await self.room.local_participant.publish_data(
                            data, 
                            topic="lk.transcription"
                        )

                        # Save using the computed start timestamp
                        # save_to_backend expects int64. We now send Milliseconds for precision.
                        # Compute the skew-free offset from recording start in
                        # the agent's OWN clock (final_timestamp_ms is agent
                        # wall-clock ms), so playback seeking is accurate.
                        rec_start_s, _rec_egress = self._recording_anchor_s()
                        offset_ms = None
                        if rec_start_s is not None:
                            offset_ms = int(max(0, final_timestamp_ms - int(rec_start_s * 1000)))
                        asyncio.create_task(self.save_to_backend(participant.identity, text, timestamp=int(final_timestamp_ms), offset_ms=offset_ms))
                        
            await asyncio.gather(push_audio(), receive_results())
            
        except Exception as e:
            logger.error(f"Transcription error for {participant.identity}: {e}")
            import traceback
            traceback.print_exc() # Print full stack for deep dive
        finally:
            if stt_stream:
                await stt_stream.aclose()
            logger.info(f"Stopped transcription for {participant.identity}")

    def _call_session_key(self):
        """Name this call, for a transcript that has no recording to hang off.

        LiveKit issues a room sid per call rather than per room, so this names
        exactly the call in progress and the next call in the same channel gets
        a different one. Must stay in step with helpers.CallSessionKey on the
        backend, which is what decides that a key like this has no media behind
        it and must never appear in a list of recordings.
        """
        sid = getattr(self.room, "sid", "") or ""
        # room.sid is a coroutine property on some client versions; only a
        # plain string is usable here, and guessing wrong would produce a key
        # like "call-<coroutine object ...>" that differs on every line.
        if not isinstance(sid, str) or not sid.strip():
            return ""
        return "call-" + sid.strip()

    async def save_to_backend(self, identity, text, timestamp=None, offset_ms=None):
        backend_url = os.getenv("BACKEND_URL")
        # Internal-service auth: the BE requires X-Internal-Secret
        # matching its INTERNAL_SECRET env var. Without it the request
        # is rejected with 401. Inject it on every transcript post.
        internal_secret = os.getenv("INTERNAL_SECRET", "")
        
        if backend_url:
            try:
                # Egress ID from metadata
                egress_id = ""
                if self.room.metadata:
                     try:
                        meta = json.loads(self.room.metadata)
                        egress_id = meta.get("egressID", "")
                     except: pass

                # A call nobody recorded still produces a transcript worth
                # keeping. This used to `return` here, which is the whole reason
                # meeting recaps only ever appeared for recorded calls: the
                # words were transcribed for the live captions and then thrown
                # away. The backend keys transcript lines by this id, so an
                # unrecorded call sends its SESSION key instead of a recording's.
                transcript_key = egress_id or self._call_session_key()
                if not transcript_key:
                    # No recording and no session id to stand in for one. Better
                    # to drop the line than to file it under a key shared with
                    # every other call in the workspace.
                    logger.warning("No egress id and no room session id; dropping a transcript line")
                    return

                # Use shared session
                payload = {
                    "room_name": self.room.name,
                    "participant_identity": identity,
                    "text": text,
                    "timestamp": timestamp if timestamp is not None else int(time.time()),
                    "egress_id": transcript_key
                }
                # offset_ms is the skew-free seek anchor (ms from recording
                # start, in the producer's own clock). Only include it when
                # known so older/legacy rows fall back to timestamp math.
                if offset_ms is not None:
                    payload["offset_ms"] = int(max(0, offset_ms))
                headers = {}
                if internal_secret:
                    headers["X-Internal-Secret"] = internal_secret
                async with self.http_session.post(backend_url, json=payload, headers=headers) as resp:
                    if resp.status != 200:
                        logger.warning(f"Backend save failed: {resp.status}")
                    else:
                        logger.debug(f"Saved transcript for {identity}: {text[:20]}...")
            except Exception as e:
                logger.error(f"Save error: {e}")

    async def stop(self):
        self.shutdown_flag.set()
        await self.room.disconnect()
        self.cleanup_cb(self.room_name)

async def main():
    service = TranscriptionService()
    
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, service.shutdown_event.set)
        
    await service.start()

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass
