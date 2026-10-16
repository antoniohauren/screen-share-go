#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#include <windows.h>
#include <audioclient.h>
#include <audioclientactivationparams.h>
#include <mmdeviceapi.h>
#include <wrl.h>
#include <wrl/implements.h>
#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cwchar>
#include <limits>
#include <stdexcept>
#include <string>
#include <vector>

using Microsoft::WRL::ComPtr;
using Microsoft::WRL::FtmBase;
using Microsoft::WRL::RuntimeClass;
using Microsoft::WRL::RuntimeClassFlags;
using Microsoft::WRL::ClassicCom;

static void check(HRESULT hr, const char* what) {
    if (FAILED(hr)) {
        char text[240];
        std::snprintf(text, sizeof(text), "%s failed (HRESULT 0x%08lX)", what, static_cast<unsigned long>(hr));
        throw std::runtime_error(text);
    }
}

struct Handle {
    HANDLE value = nullptr;
    explicit Handle(HANDLE h) : value(h) {
        if (!h || h == INVALID_HANDLE_VALUE) throw std::runtime_error("Windows handle creation failed");
    }
    ~Handle() { CloseHandle(value); }
    Handle(const Handle&) = delete;
    Handle& operator=(const Handle&) = delete;
};

class Activation final : public RuntimeClass<RuntimeClassFlags<ClassicCom>, IActivateAudioInterfaceCompletionHandler, FtmBase> {
public:
    Handle complete{CreateEventW(nullptr, TRUE, FALSE, nullptr)};
    HRESULT result = E_PENDING;
    ComPtr<IAudioClient> client;

    STDMETHODIMP ActivateCompleted(IActivateAudioInterfaceAsyncOperation* operation) override {
        ComPtr<IUnknown> object;
        HRESULT activated = E_FAIL;
        result = operation->GetActivateResult(&activated, &object);
        if (SUCCEEDED(result)) result = activated;
        if (SUCCEEDED(result)) result = object.As(&client);
        SetEvent(complete.value);
        return S_OK;
    }
};

static ComPtr<IAudioClient> activateProcess(DWORD pid) {
    // INCLUDE_TARGET_PROCESS_TREE is essential: never retry with system loopback.
    AUDIOCLIENT_ACTIVATION_PARAMS params{};
    params.ActivationType = AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK;
    params.ProcessLoopbackParams.TargetProcessId = pid;
    params.ProcessLoopbackParams.ProcessLoopbackMode = PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE;
    PROPVARIANT property{};
    property.vt = VT_BLOB;
    property.blob.cbSize = sizeof(params);
    property.blob.pBlobData = reinterpret_cast<BYTE*>(&params);
    auto completion = Microsoft::WRL::Make<Activation>();
    if (!completion) throw std::runtime_error("Cannot allocate activation callback");
    ComPtr<IActivateAudioInterfaceAsyncOperation> operation;
    check(ActivateAudioInterfaceAsync(VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK, __uuidof(IAudioClient), &property, completion.Get(), &operation), "Activate process-tree playback loopback (Windows 11 required)");
    if (WaitForSingleObject(completion->complete.value, 10000) != WAIT_OBJECT_0)
        throw std::runtime_error("Process loopback activation timed out");
    check(completion->result, "Activate process-tree playback loopback; no system-audio fallback");
    return completion->client;
}

static ComPtr<IAudioClient> activateSystem() {
    ComPtr<IMMDeviceEnumerator> devices;
    check(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL, IID_PPV_ARGS(&devices)), "Create playback-device enumerator");
    ComPtr<IMMDevice> device;
    // eRender only. No eCapture endpoint and no microphone code exists.
    check(devices->GetDefaultAudioEndpoint(eRender, eConsole, &device), "Get default playback device");
    ComPtr<IAudioClient> client;
    check(device->Activate(__uuidof(IAudioClient), CLSCTX_ALL, nullptr, &client), "Activate playback loopback");
    return client;
}

static uint64_t clock100ns() {
    LARGE_INTEGER now, frequency;
    QueryPerformanceCounter(&now); QueryPerformanceFrequency(&frequency);
    return static_cast<uint64_t>((static_cast<long double>(now.QuadPart) * 10000000) / frequency.QuadPart);
}

static void writeAll(const void* data, DWORD bytes) {
    HANDLE output = GetStdHandle(STD_OUTPUT_HANDLE);
    const auto* p = static_cast<const BYTE*>(data);
    while (bytes) {
        DWORD written = 0;
        if (!WriteFile(output, p, bytes, &written, nullptr) || written == 0)
            throw std::runtime_error("PCM output pipe closed");
        p += written; bytes -= written;
    }
}

static void capture(ComPtr<IAudioClient> client, HANDLE process, HWND window = nullptr, DWORD pid = 0) {
    WAVEFORMATEX format{};
    format.wFormatTag = WAVE_FORMAT_PCM;
    format.nChannels = 2;
    format.nSamplesPerSec = 48000;
    format.wBitsPerSample = 16;
    format.nBlockAlign = 4;
    format.nAvgBytesPerSec = 192000;
    Handle event(CreateEventW(nullptr, FALSE, FALSE, nullptr));
    check(client->Initialize(AUDCLNT_SHAREMODE_SHARED,
        AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK |
        AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY,
        0, 0, &format, nullptr), "Initialize stereo 48 kHz playback loopback");
    check(client->SetEventHandle(event.value), "Set capture event");
    ComPtr<IAudioCaptureClient> packets;
    check(client->GetService(IID_PPV_ARGS(&packets)), "Get playback capture service");

    // Clocked ring writes silence through idle playback periods. Timestamped packets
    // land 60 ms before playout, so stopping/starting sound cannot accumulate A/V drift.
    constexpr uint64_t capacity = 96000;
    constexpr uint64_t delay = 2880;
    std::vector<int16_t> ring(capacity * 2, 0);
    uint64_t outputFrame = 0;
    const uint64_t origin = clock100ns();
    check(client->Start(), "Start playback loopback");
    try {
        for (;;) {
            if (process && WaitForSingleObject(process, 0) == WAIT_OBJECT_0)
                throw std::runtime_error("Selected window's owning process exited; capture stopped");
            if (window) {
                DWORD owner = 0;
                GetWindowThreadProcessId(window, &owner);
                if (!IsWindow(window) || owner != pid || IsIconic(window))
                    throw std::runtime_error("Selected window closed, changed owner, or minimized; capture stopped");
            }
            DWORD wait = WaitForSingleObject(event.value, 10);
            if (wait != WAIT_OBJECT_0 && wait != WAIT_TIMEOUT)
                throw std::runtime_error("Capture event wait failed");
            UINT32 frames = 0;
            check(packets->GetNextPacketSize(&frames), "Read playback packet size");
            while (frames) {
                BYTE* data = nullptr;
                DWORD flags = 0;
                UINT64 position = 0, timestamp = 0;
                check(packets->GetBuffer(&data, &frames, &flags, &position, &timestamp), "Read playback packet");
                if (flags & AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR) {
                    packets->ReleaseBuffer(frames);
                    throw std::runtime_error("Playback device returned invalid timing; capture stopped");
                }
                const int64_t first = timestamp >= origin
                    ? static_cast<int64_t>((timestamp - origin) * 48000 / 10000000)
                    : -static_cast<int64_t>((origin - timestamp) * 48000 / 10000000);
                for (UINT32 i = 0; i < frames; ++i) {
                    const int64_t frame = first + i;
                    if (frame < static_cast<int64_t>(outputFrame)) continue;
                    if (frame >= static_cast<int64_t>(outputFrame + capacity)) {
                        packets->ReleaseBuffer(frames);
                        throw std::runtime_error("Playback clock discontinuity; capture stopped");
                    }
                    const auto slot = (static_cast<uint64_t>(frame) % capacity) * 2;
                    if (flags & AUDCLNT_BUFFERFLAGS_SILENT) { ring[slot] = 0; ring[slot+1] = 0; }
                    else {
                        const auto* samples = reinterpret_cast<const int16_t*>(data);
                        ring[slot] = samples[i*2]; ring[slot+1] = samples[i*2+1];
                    }
                }
                check(packets->ReleaseBuffer(frames), "Release playback packet");
                check(packets->GetNextPacketSize(&frames), "Read playback packet size");
            }
            const uint64_t elapsed = (clock100ns() - origin) * 48000 / 10000000;
            const uint64_t target = elapsed > delay ? elapsed - delay : 0;
            if (target > outputFrame + capacity) throw std::runtime_error("Audio encoder stalled; capture stopped");
            int16_t block[480*2];
            while (outputFrame + 480 <= target) {
                for (uint64_t i = 0; i < 480; ++i) {
                    const auto slot = ((outputFrame+i) % capacity) * 2;
                    block[i*2] = ring[slot]; block[i*2+1] = ring[slot+1];
                    ring[slot] = 0; ring[slot+1] = 0;
                }
                writeAll(block, sizeof(block)); outputFrame += 480;
            }
        }
    } catch (...) { client->Stop(); throw; }
}

int wmain(int argc, wchar_t** argv) {
    bool initialized = false;
    try {
        bool system = argc == 2 && std::wcscmp(argv[1], L"--system") == 0;
        DWORD pid = 0;
        HWND window = nullptr;
        if (!system) {
            if (argc != 5 || std::wcscmp(argv[1], L"--process") != 0 || !argv[2][0] || std::wcscmp(argv[3], L"--window") != 0 || !argv[4][0])
                throw std::runtime_error("Usage: capture-audio.exe --system | --process PID --window HWND");
            uint64_t value = 0;
            for (const wchar_t* p = argv[2]; *p; ++p) {
                if (*p < L'0' || *p > L'9') throw std::runtime_error("Invalid process ID");
                value = value * 10 + (*p - L'0');
                if (value > std::numeric_limits<DWORD>::max()) throw std::runtime_error("Invalid process ID");
            }
            if (!value) throw std::runtime_error("Invalid process ID");
            pid = static_cast<DWORD>(value);
            value = 0;
            for (const wchar_t* p = argv[4]; *p; ++p) {
                if (*p < L'0' || *p > L'9' || value > (UINT64_MAX - (*p-L'0')) / 10)
                    throw std::runtime_error("Invalid window handle");
                value = value * 10 + (*p - L'0');
            }
            if (!value) throw std::runtime_error("Invalid window handle");
            window = reinterpret_cast<HWND>(static_cast<uintptr_t>(value));
        }
        check(CoInitializeEx(nullptr, COINIT_MULTITHREADED), "Initialize COM"); initialized = true;
        if (system) capture(activateSystem(), nullptr);
        else {
            // Hold process identity open; never reinterpret a recycled PID on failure.
            Handle process(OpenProcess(SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid));
            if (WaitForSingleObject(process.value, 0) != WAIT_TIMEOUT)
                throw std::runtime_error("Selected process has exited");
            DWORD owner = 0;
            GetWindowThreadProcessId(window, &owner);
            if (owner != pid || !IsWindow(window)) throw std::runtime_error("Selected window changed owner or closed");
            capture(activateProcess(pid), process.value, window, pid);
        }
        CoUninitialize();
        return 0;
    } catch (const std::exception& error) {
        std::fprintf(stderr, "Audio capture: %s\n", error.what());
        if (initialized) CoUninitialize();
        return 1;
    }
}
