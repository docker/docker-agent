//go:build darwin && cgo && !no_audio

package capture

/*
#cgo LDFLAGS: -framework CoreAudio -framework AudioToolbox -framework CoreFoundation

#include <AudioToolbox/AudioToolbox.h>
#include <errno.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define CHANNELS 1
#define BITS_PER_SAMPLE 16
#define NUM_BUFFERS 3

typedef struct {
    AudioQueueRef queue;
    AudioFileID file;
    AudioStreamBasicDescription format;
    SInt64 packet;
    uintptr_t handle;
    pthread_mutex_t mutex;
    bool started;
    bool stopping;
    OSStatus error;
} Capture;

void goAudioCallback(uintptr_t handle, void *data, int32_t size, int32_t status);

static void AudioInputCallback(
    void *userData,
    AudioQueueRef queue,
    AudioQueueBufferRef buffer,
    const AudioTimeStamp *startTime,
    UInt32 packetCount,
    const AudioStreamPacketDescription *packetDescriptions
) {
    Capture *capture = userData;
    pthread_mutex_lock(&capture->mutex);
    if (capture->error != noErr) {
        pthread_mutex_unlock(&capture->mutex);
        return;
    }

    if (buffer->mAudioDataByteSize > 0) {
        if (capture->file != NULL) {
            UInt32 packets = buffer->mAudioDataByteSize / capture->format.mBytesPerPacket;
            capture->error = AudioFileWritePackets(capture->file, false,
                buffer->mAudioDataByteSize, NULL, capture->packet, &packets, buffer->mAudioData);
            capture->packet += packets;
        }
        if (capture->error == noErr) {
            goAudioCallback(capture->handle, buffer->mAudioData, (int)buffer->mAudioDataByteSize, noErr);
        }
    }

    if (!capture->stopping && capture->error == noErr) {
        capture->error = AudioQueueEnqueueBuffer(queue, buffer, 0, NULL);
    }
    if (capture->error != noErr) {
        goAudioCallback(capture->handle, NULL, 0, capture->error);
    }
    pthread_mutex_unlock(&capture->mutex);
}

static OSStatus freeCapture(Capture *capture) {
    pthread_mutex_lock(&capture->mutex);
    capture->stopping = true;
    pthread_mutex_unlock(&capture->mutex);

    OSStatus status = noErr;
    if (capture->queue != NULL) {
        // Stop delivers partially filled buffers before disposal joins callbacks.
        if (capture->started) status = AudioQueueStop(capture->queue, true);
        OSStatus disposeStatus = AudioQueueDispose(capture->queue, true);
        if (status == noErr) status = disposeStatus;
    }
    if (capture->file != NULL) {
        OSStatus fileStatus = AudioFileClose(capture->file);
        if (status == noErr) status = fileStatus;
    }
    if (status == noErr) status = capture->error;
    pthread_mutex_destroy(&capture->mutex);
    free(capture);
    return status;
}

static Capture *startCapture(const char *filePath, double rate, uintptr_t handle, OSStatus *status) {
    Capture *capture = calloc(1, sizeof(Capture));
    if (capture == NULL) {
        *status = ENOMEM;
        return NULL;
    }
    int mutexStatus = pthread_mutex_init(&capture->mutex, NULL);
    if (mutexStatus != 0) {
        *status = mutexStatus;
        free(capture);
        return NULL;
    }
    capture->handle = handle;
    capture->format.mSampleRate = rate;
    capture->format.mFormatID = kAudioFormatLinearPCM;
    capture->format.mFormatFlags = kLinearPCMFormatFlagIsSignedInteger | kLinearPCMFormatFlagIsPacked;
    capture->format.mFramesPerPacket = 1;
    capture->format.mChannelsPerFrame = CHANNELS;
    capture->format.mBitsPerChannel = BITS_PER_SAMPLE;
    capture->format.mBytesPerPacket = (BITS_PER_SAMPLE / 8) * CHANNELS;
    capture->format.mBytesPerFrame = (BITS_PER_SAMPLE / 8) * CHANNELS;

    if (filePath != NULL) {
        CFURLRef url = CFURLCreateFromFileSystemRepresentation(
            NULL, (const UInt8 *)filePath, strlen(filePath), false);
        if (url == NULL) {
            *status = EINVAL;
            goto fail;
        }
        *status = AudioFileCreateWithURL(url, kAudioFileWAVEType,
            &capture->format, kAudioFileFlags_EraseFile, &capture->file);
        CFRelease(url);
        if (*status != noErr) goto fail;
    }

    *status = AudioQueueNewInput(&capture->format, AudioInputCallback, capture,
        NULL, kCFRunLoopCommonModes, 0, &capture->queue);
    if (*status != noErr) goto fail;

    UInt32 bufferSize = (UInt32)(rate * 0.1) * (BITS_PER_SAMPLE / 8) * CHANNELS;
    for (int i = 0; i < NUM_BUFFERS; i++) {
        AudioQueueBufferRef buffer;
        *status = AudioQueueAllocateBuffer(capture->queue, bufferSize, &buffer);
        if (*status != noErr) goto fail;
        *status = AudioQueueEnqueueBuffer(capture->queue, buffer, 0, NULL);
        if (*status != noErr) goto fail;
    }
    *status = AudioQueueStart(capture->queue, NULL);
    if (*status != noErr) goto fail;
    capture->started = true;
    return capture;

fail:
    freeCapture(capture);
    return NULL;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime/cgo"
	"strings"
	"unsafe"
)

type callbacks struct {
	handler AudioHandler
	errors  chan int
}

//export goAudioCallback
func goAudioCallback(handle uintptr, data unsafe.Pointer, size, status int32) {
	cb := cgo.Handle(handle).Value().(*callbacks)
	if status != 0 {
		select {
		case cb.errors <- int(status):
		default:
		}
		return
	}
	if cb.handler != nil {
		cb.handler(C.GoBytes(data, C.int(size)))
	}
}

// Run captures until ctx is canceled. A non-empty filePath also records a WAV file.
// Each invocation is independent. The optional ready callback runs after capture starts.
// Handlers run synchronously; Run waits for them before returning, so they must not wait for Run.
// Canceling ctx normally returns nil. Native capture and cleanup failures return an error.
func (c *Capturer) Run(ctx context.Context, filePath string, handler AudioHandler, ready func()) (err error) {
	if ctx.Err() != nil {
		return nil
	}
	// C.GoBytes takes a signed 32-bit size for the 100 ms PCM buffer.
	if c.sampleRate < 10 || uint64(c.sampleRate/10)*2 > 1<<31-1 {
		return fmt.Errorf("invalid audio sample rate: %d", c.sampleRate)
	}
	if strings.IndexByte(filePath, 0) >= 0 {
		return errors.New("audio file path contains a NUL byte")
	}

	cb := &callbacks{handler: handler, errors: make(chan int, 1)}
	handle := cgo.NewHandle(cb)
	defer handle.Delete()

	var path *C.char
	if filePath != "" {
		path = C.CString(filePath)
		defer C.free(unsafe.Pointer(path))
	}
	var status C.OSStatus
	capture := C.startCapture(path, C.double(c.sampleRate), C.uintptr_t(handle), &status)
	if capture == nil {
		return fmt.Errorf("start audio capture: OSStatus %d", status)
	}
	defer func() {
		// Native callbacks must finish before their Go handle is deleted.
		if status := C.freeCapture(capture); status != 0 {
			err = fmt.Errorf("audio capture: OSStatus %d", status)
		}
	}()

	if ready != nil {
		ready()
	}
	select {
	case <-ctx.Done():
	case <-cb.errors:
	}
	return nil
}
