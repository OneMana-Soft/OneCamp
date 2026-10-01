import asyncio
try:
    from livekit import rtc
    print("Successfully imported rtc")
    
    # Check for AudioFrameEvent
    if hasattr(rtc, 'AudioFrameEvent'):
        print("Found AudioFrameEvent in rtc")
        # We can't easily inspect an instance without running a room, but we can check the class
        print(f"AudioFrameEvent dir: {dir(rtc.AudioFrameEvent)}")
    else:
        print("AudioFrameEvent not in rtc module level")

    if hasattr(rtc, 'AudioStream'):
         print(f"AudioStream dir: {dir(rtc.AudioStream)}")

except ImportError as e:
    print(f"ImportError: {e}")
except Exception as e:
    print(f"An error occurred: {e}")
