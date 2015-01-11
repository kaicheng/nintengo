package nes

import (
	"bufio"
	"errors"
	"fmt"
	"log"

	"os"
	"runtime"
	"runtime/pprof"

	"encoding/json"

	"archive/zip"

	"github.com/nwidger/nintengo/m65go2"
	"github.com/nwidger/nintengo/rp2ago3"
	"github.com/nwidger/nintengo/rp2cgo2"
)

//go:generate stringer -type=StepState
type StepState uint8

const (
	NoStep StepState = iota
	CycleStep
	ScanlineStep
	FrameStep
)

//go:generate stringer -type=RunState
type RunState uint8

const (
	Running RunState = iota
	Quitting
)

type PauseRequest uint8

const (
	Toggle PauseRequest = iota
	Pause
	Unpause
)

type NES struct {
	state         RunState
	frameStep     StepState
	paused        chan *PauseEvent
	events        chan Event
	CPU           *rp2ago3.RP2A03
	cpuDivisor    float32
	PPU           *rp2cgo2.RP2C02
	PPUQuota      float32
	controllers   *Controllers
	ROM           ROM
	audio         Audio
	video         Video
	fps           *FPS
	recorder      Recorder
	audioRecorder AudioRecorder
	options       *Options
}

type Options struct {
	Recorder      string
	AudioRecorder string
	CPUDecode     bool
	CPUProfile    string
	MemProfile    string
	HTTPAddress   string
}

func NewNES(filename string, options *Options) (nes *NES, err error) {
	var audio Audio
	var video Video
	var recorder Recorder
	var audioRecorder AudioRecorder
	var cpuDivisor float32

	audioFrequency := 44100
	audioSampleSize := 2048

	cpu := rp2ago3.NewRP2A03(audioFrequency)

	if options.CPUDecode {
		cpu.EnableDecode()
	}

	ppu := rp2cgo2.NewRP2C02(cpu.InterruptLine(m65go2.Nmi))

	rom, err := NewROM(filename, cpu.InterruptLine(m65go2.Irq), ppu.Nametable.SetTables)

	if err != nil {
		err = errors.New(fmt.Sprintf("Error loading ROM: %v", err))
		return
	}

	switch rom.Region() {
	case NTSC:
		cpuDivisor = rp2ago3.NTSC_CPU_CLOCK_DIVISOR
	case PAL:
		cpuDivisor = rp2ago3.PAL_CPU_CLOCK_DIVISOR
	}

	ctrls := NewControllers()

	events := make(chan Event)
	video, err = NewVideo(rom.GameName(), events)

	if err != nil {
		err = errors.New(fmt.Sprintf("Error creating video: %v", err))
		return
	}

	audio, err = NewAudio(audioFrequency, audioSampleSize)

	if err != nil {
		err = errors.New(fmt.Sprintf("Error creating audio: %v", err))
		return
	}

	switch options.Recorder {
	case "none":
		// none
	case "jpeg":
		recorder, err = NewJPEGRecorder()
	case "gif":
		recorder, err = NewGIFRecorder()
	}

	if err != nil {
		err = errors.New(fmt.Sprintf("Error creating recorder: %v", err))
		return
	}

	switch options.AudioRecorder {
	case "none":
		// none
	case "wav":
		audioRecorder, err = NewWAVRecorder()
	}

	if err != nil {
		err = errors.New(fmt.Sprintf("Error creating audio recorder: %v", err))
		return
	}

	cpu.Memory.AddMappings(ppu, rp2ago3.CPU)
	cpu.Memory.AddMappings(rom, rp2ago3.CPU)
	cpu.Memory.AddMappings(ctrls, rp2ago3.CPU)

	ppu.Memory.AddMappings(rom, rp2ago3.PPU)

	nes = &NES{
		frameStep:     NoStep,
		paused:        make(chan *PauseEvent),
		events:        events,
		CPU:           cpu,
		cpuDivisor:    cpuDivisor,
		PPU:           ppu,
		ROM:           rom,
		audio:         audio,
		video:         video,
		fps:           NewFPS(DEFAULT_FPS),
		recorder:      recorder,
		audioRecorder: audioRecorder,
		controllers:   ctrls,
		options:       options,
	}

	return
}

func (nes *NES) Reset() {
	nes.CPU.Reset()
	nes.PPU.Reset()
	nes.PPUQuota = float32(0)
	nes.controllers.Reset()
}

func (nes *NES) RunState() RunState {
	return nes.state
}

func (nes *NES) StepState() StepState {
	return nes.frameStep
}

func (nes *NES) Pause() RunState {
	e := &PauseEvent{}
	e.Process(nes)

	return nes.state
}

func (nes *NES) SaveState() {
	name := nes.ROM.GameName() + ".nst"

	fo, err := os.Create(name)
	defer fo.Close()

	if err != nil {
		fmt.Printf("*** Error saving state: %s\n", err)
		return
	}

	w := bufio.NewWriter(fo)
	defer w.Flush()

	zw := zip.NewWriter(w)
	defer zw.Close()

	vfw, err := zw.Create("meta.json")

	if err != nil {
		fmt.Printf("*** Error saving state: %s\n", err)
		return
	}

	enc := json.NewEncoder(vfw)

	if err = enc.Encode(struct{ Version string }{"0.2"}); err != nil {
		fmt.Printf("*** Error saving state: %s\n", err)
		return
	}

	zfw, err := zw.Create("state.json")

	if err != nil {
		fmt.Printf("*** Error saving state: %s\n", err)
		return
	}

	buf, err := json.MarshalIndent(nes, "", "  ")

	if _, err = zfw.Write(buf); err != nil {
		fmt.Printf("*** Error saving state: %s\n", err)
		return
	}

	fmt.Println("*** Saving state to", name)
}

func (nes *NES) LoadState() {
	name := nes.ROM.GameName() + ".nst"

	zr, err := zip.OpenReader(name)
	defer zr.Close()

	if err != nil {
		fmt.Printf("*** Error loading state: %s\n", err)
		return
	}

	loaded := false

	for _, zf := range zr.File {
		switch zf.Name {
		case "meta.json":
			zfr, err := zf.Open()
			defer zfr.Close()

			if err != nil {
				fmt.Printf("*** Error loading state: %s\n", err)
				return
			}

			dec := json.NewDecoder(zfr)

			v := struct{ Version string }{}

			if err = dec.Decode(&v); err != nil {
				fmt.Printf("*** Error loading state: %s\n", err)
				return
			}

			if v.Version != "0.2" {
				fmt.Printf("*** Error loading state: Invalid save state format version '%s'\n", v.Version)
				return
			}
		case "state.json":
			zfr, err := zf.Open()
			defer zfr.Close()

			if err != nil {
				fmt.Printf("*** Error loading state: %s\n", err)
				return
			}

			dec := json.NewDecoder(zfr)

			if err = dec.Decode(nes); err != nil {
				fmt.Printf("*** Error loading state: %s\n", err)
				return
			}

			loaded = true
		}
	}

	if !loaded {
		fmt.Printf("*** Error loading state: invalid save state file\n")
		return
	}

	fmt.Println("*** Loading state from", name)
}

func (nes *NES) processEvents() {
	for nes.state != Quitting {
		e := <-nes.events
		e.Process(nes)
	}
}

func (nes *NES) runProcessors() (err error) {
	var cycles uint16

	isPaused := false
	mmc3, _ := nes.ROM.(*MMC3)

	for nes.state != Quitting {
		if nes.PPUQuota < 1.0 {
			if cycles, err = nes.CPU.Execute(); err != nil {
				break
			}

			nes.PPUQuota += float32(cycles) * nes.cpuDivisor
		}

		if nes.PPUQuota >= 1.0 {
			scanline := nes.PPU.Scanline

			if colors := nes.PPU.Execute(); colors != nil {
				nes.frame(colors)
				nes.fps.Delay()

				if nes.frameStep == FrameStep {
					isPaused = true
					fmt.Println("*** Paused at frame", nes.PPU.Frame)
				}
			}

			if mmc3 != nil && nes.PPU.TriggerScanlineCounter() {
				mmc3.scanlineCounter()
			}

			nes.PPUQuota--

			if nes.frameStep == CycleStep ||
				(nes.frameStep == ScanlineStep && nes.PPU.Scanline != scanline) {
				isPaused = true

				if nes.frameStep == CycleStep {
					fmt.Println("*** Paused at cycle", nes.PPU.Cycle)
				} else {
					fmt.Println("*** Paused at scanline", nes.PPU.Scanline)
				}
			}
		}

		if nes.PPUQuota < 1.0 {
			for i := uint16(0); i < cycles; i++ {
				if sample, haveSample := nes.CPU.APU.Execute(); haveSample {
					nes.sample(sample)
				}
			}
		}

		select {
		case pr := <-nes.paused:
			isPaused = nes.isPaused(pr, isPaused)
		default:
		}

		for isPaused {
			isPaused = nes.isPaused(<-nes.paused, isPaused)
		}
	}

	return
}

func (nes *NES) isPaused(pr *PauseEvent, oldPaused bool) (isPaused bool) {
	switch pr.request {
	case Pause:
		isPaused = true
	case Unpause:
		isPaused = false
	case Toggle:
		isPaused = !oldPaused
	}

	if pr.changed != nil {
		pr.changed <- (isPaused != oldPaused)
	}

	return
}

func (nes *NES) frame(colors []uint8) {
	e := &FrameEvent{
		colors: colors,
	}

	e.Process(nes)
}

func (nes *NES) sample(sample int16) {
	e := &SampleEvent{
		sample: sample,
	}

	e.Process(nes)
}

func (nes *NES) Run() (err error) {
	fmt.Println(nes.ROM)

	nes.ROM.LoadBattery()
	nes.Reset()

	nes.state = Running

	go nes.audio.Run()
	go nes.processEvents()

	go func() {
		if err := nes.runProcessors(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	}()

	if nes.recorder != nil {
		go nes.recorder.Run()
	}

	if nes.audioRecorder != nil {
		go nes.audioRecorder.Run()
	}

	runtime.LockOSThread()
	runtime.GOMAXPROCS(runtime.NumCPU())

	if nes.options.CPUProfile != "" {
		f, err := os.Create(nes.options.CPUProfile)

		if err != nil {
			log.Fatal(err)
		}

		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	nes.video.Run()

	if nes.recorder != nil {
		nes.recorder.Quit()
	}

	if nes.audioRecorder != nil {
		nes.audioRecorder.Quit()
	}

	if nes.options.MemProfile != "" {
		f, err := os.Create(nes.options.MemProfile)

		if err != nil {
			log.Fatal(err)
		}

		pprof.WriteHeapProfile(f)
		f.Close()
	}

	err = nes.ROM.SaveBattery()

	return
}
