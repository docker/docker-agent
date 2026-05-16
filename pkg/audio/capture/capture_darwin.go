//go:build darwin && !no_audio

// Package capture provides audio capture functionality for macOS using CoreAudio.
package capture

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreAudio -framework AudioToolbox -framework CoreFoundation

#include <AudioToolbox/AudioToolbox.h>
#include <CoreAudio/CoreAudio.h>
#include <stdlib.h>
#include <string.h>

#define CHANNELS 1
#define BITS_PER_SAMPLE 16
#define NUM_BUFFERS 3

// Capture state
static AudioQueueRef audioQueue = NULL;
static AudioQueueBufferRef buffers[NUM_BUFFERS];
static AudioFileID audioFile = NULL;
static SInt64 currentPacket = 0;
static volatile int isCapturing = 0;
static double sampleRate = 44100.0;
static UInt32 bufferSize = 4096;

// Audio format description
static AudioStreamBasicDescription audioFormat;

// Forward declaration for Go callback
void goAudioCallback(void *data, int size);

// Callback function for audio input
static void AudioInputCallback(
    void *inUserData,
    AudioQueueRef inAQ,
    AudioQueueBufferRef inBuffer,
    const AudioTimeStamp *inStartTime,
    UInt32 inNumberPacketDescriptions,
    const AudioStreamPacketDescription *inPacketDescs
) {
    if (!isCapturing || inBuffer->mAudioDataByteSize == 0) return;

    // Write to file if recording
    if (audioFile != NULL) {
        UInt32 numPackets = inBuffer->mAudioDataByteSize / audioFormat.mBytesPerPacket;
        if (AudioFileWritePackets(audioFile, false, inBuffer->mAudioDataByteSize,
                                   NULL, currentPacket, &numPackets, inBuffer->mAudioData) == noErr) {
            currentPacket += numPackets;
        }
    }

    // Call Go callback for streaming
    goAudioCallback(inBuffer->mAudioData, (int)inBuffer->mAudioDataByteSize);

    if (isCapturing) {
        AudioQueueEnqueueBuffer(inAQ, inBuffer, 0, NULL);
    }
}

// Initialize audio format with given sample rate
static void initAudioFormat(double rate) {
    sampleRate = rate;
    bufferSize = (UInt32)(rate * 0.1) * (BITS_PER_SAMPLE / 8) * CHANNELS; // 100ms buffer

    memset(&audioFormat, 0, sizeof(audioFormat));
    audioFormat.mSampleRate = rate;
    audioFormat.mFormatID = kAudioFormatLinearPCM;
    audioFormat.mFormatFlags = kLinearPCMFormatFlagIsSignedInteger | kLinearPCMFormatFlagIsPacked;
    audioFormat.mFramesPerPacket = 1;
    audioFormat.mChannelsPerFrame = CHANNELS;
    audioFormat.mBitsPerChannel = BITS_PER_SAMPLE;
    audioFormat.mBytesPerPacket = (BITS_PER_SAMPLE / 8) * CHANNELS;
    audioFormat.mBytesPerFrame = (BITS_PER_SAMPLE / 8) * CHANNELS;
}

// Start capturing audio, optionally recording to file
// filePath can be NULL for streaming-only mode
static int startCapture(const char *filePath, double rate) {
    if (isCapturing) return -1;

    initAudioFormat(rate);

    // Create audio file if path provided
    if (filePath != NULL) {
        CFURLRef fileURL = CFURLCreateFromFileSystemRepresentation(
            NULL, (const UInt8 *)filePath, strlen(filePath), false);
        if (fileURL == NULL) return -2;

        OSStatus status = AudioFileCreateWithURL(fileURL, kAudioFileWAVEType,
            &audioFormat, kAudioFileFlags_EraseFile, &audioFile);
        CFRelease(fileURL);
        if (status != noErr) return -3;
    }

    // Create audio queue
    OSStatus status = AudioQueueNewInput(&audioFormat, AudioInputCallback, NULL,
        NULL, kCFRunLoopCommonModes, 0, &audioQueue);
    if (status != noErr) {
        if (audioFile) { AudioFileClose(audioFile); audioFile = NULL; }
        return -4;
    }

    // Allocate and enqueue buffers
    for (int i = 0; i < NUM_BUFFERS; i++) {
        status = AudioQueueAllocateBuffer(audioQueue, bufferSize, &buffers[i]);
        if (status != noErr) {
            AudioQueueDispose(audioQueue, true);
            if (audioFile) { AudioFileClose(audioFile); audioFile = NULL; }
            return -5;
        }
        AudioQueueEnqueueBuffer(audioQueue, buffers[i], 0, NULL);
    }

    currentPacket = 0;
    isCapturing = 1;

    status = AudioQueueStart(audioQueue, NULL);
    if (status != noErr) {
        isCapturing = 0;
        AudioQueueDispose(audioQueue, true);
        if (audioFile) { AudioFileClose(audioFile); audioFile = NULL; }
        return -6;
    }

    return 0;
}

// Stop capturing
static int stopCapture() {
    if (!isCapturing) return -1;

    isCapturing = 0;
    AudioQueueStop(audioQueue, true);
    AudioQueueDispose(audioQueue, true);
    audioQueue = NULL;

    if (audioFile != NULL) {
        AudioFileClose(audioFile);
        audioFile = NULL;
    }

    return 0;
}
*/
import "C"

import (
	"errors"
	"sync"
	"time"
	"unsafe"
)

// Common sample rates
const (
	SampleRate44100 = 44100 // CD quality, used for WAV recording
	SampleRate24000 = 24000 // Required by OpenAI Realtime API
)

// AudioHandler is called for each chunk of captured audio data.
// The data is mono, 16-bit signed PCM (little-endian).
type AudioHandler func(data []byte)

// Capturer records audio from the microphone using CoreAudio.
type Capturer struct {
	mu         sync.Mutex
	capturing  bool
	handler    AudioHandler
	sampleRate int
}

// Global instance for C callback
var (
	globalCapturer   *Capturer
	globalCapturerMu sync.Mutex
)

//export goAudioCallback
func goAudioCallback(data unsafe.Pointer, size C.int) {
	globalCapturerMu.Lock()
	c := globalCapturer
	globalCapturerMu.Unlock()

	if c != nil && c.handler != nil {
		c.handler(C.GoBytes(data, size))
	}
}

// NewCapturer creates a new audio capturer with the specified sample rate.
func NewCapturer(sampleRate int) *Capturer {
	return &Capturer{sampleRate: sampleRate}
}

// Start begins capturing audio. If filePath is non-empty, audio is also saved to a WAV file.
// The handler is called for each chunk of audio data (can be nil if only recording to file).
func (c *Capturer) Start(filePath string, handler AudioHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.capturing {
		return errors.New("already capturing")
	}

	c.handler = handler

	globalCapturerMu.Lock()
	globalCapturer = c
	globalCapturerMu.Unlock()

	var cPath *C.char
	if filePath != "" {
		cPath = C.CString(filePath)
		defer C.free(unsafe.Pointer(cPath))
	}

	if result := C.startCapture(cPath, C.double(c.sampleRate)); result != 0 {
		return errorFromCode(int(result))
	}

	c.capturing = true
	return nil
}

// Stop ends audio capture.
func (c *Capturer) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.capturing {
		return errors.New("not capturing")
	}

	result := C.stopCapture()
	if result != 0 {
		return errorFromCode(int(result))
	}

	c.capturing = false

	globalCapturerMu.Lock()
	globalCapturer = nil
	globalCapturerMu.Unlock()

	return nil
}

// IsCapturing returns true if currently capturing.
func (c *Capturer) IsCapturing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capturing
}

// Record captures audio for the specified duration and saves to the given WAV file.
func (c *Capturer) Record(filePath string, duration time.Duration) error {
	err := c.Start(filePath, nil)
	if err != nil {
		return err
	}
	time.Sleep(duration)
	return c.Stop()
}

// Stream captures audio for the specified duration, calling handler for each chunk.
func (c *Capturer) Stream(duration time.Duration, handler AudioHandler) error {
	if err := c.Start("", handler); err != nil {
		return err
	}
	time.Sleep(duration)
	return c.Stop()
}

var errorMessages = map[int]string{
	-1: "already capturing",
	-2: "failed to create file URL",
	-3: "failed to create audio file",
	-4: "failed to create audio queue",
	-5: "failed to allocate audio buffers",
	-6: "failed to start audio queue",
}

func errorFromCode(code int) error {
	if msg, ok := errorMessages[code]; ok {
		return errors.New(msg)
	}
	return errors.New("unknown error")
}
