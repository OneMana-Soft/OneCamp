FROM python:3.11-slim

WORKDIR /app

# Install system dependencies if needed (e.g. for audio processing)
# ffmpeg is often required for audio
RUN apt-get update && apt-get install -y ffmpeg

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

CMD ["python", "agent.py", "start"]
