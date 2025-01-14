package tests

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/optimism/cannon/mipsevm"
	"github.com/ethereum-optimism/optimism/cannon/mipsevm/exec"
	"github.com/ethereum-optimism/optimism/cannon/mipsevm/memory"
	"github.com/ethereum-optimism/optimism/cannon/mipsevm/program"
	"github.com/ethereum-optimism/optimism/cannon/mipsevm/testutil"
)

func TestEVM(t *testing.T) {
	testFiles, err := os.ReadDir("open_mips_tests/test/bin")
	require.NoError(t, err)

	var tracer *tracing.Hooks // no-tracer by default, but test_util.MarkdownTracer

	cases := GetMipsVersionTestCases(t)
	skippedTests := map[string][]string{
		"multi-threaded":  []string{"clone.bin"},
		"single-threaded": []string{},
	}

	for _, c := range cases {
		skipped, exists := skippedTests[c.Name]
		require.True(t, exists)
		for _, f := range testFiles {
			testName := fmt.Sprintf("%v (%v)", f.Name(), c.Name)
			t.Run(testName, func(t *testing.T) {
				for _, skipped := range skipped {
					if f.Name() == skipped {
						t.Skipf("Skipping explicitly excluded open_mips testcase: %v", f.Name())
					}
				}

				oracle := testutil.SelectOracleFixture(t, f.Name())
				// Short-circuit early for exit_group.bin
				exitGroup := f.Name() == "exit_group.bin"
				expectPanic := strings.HasSuffix(f.Name(), "panic.bin")

				evm := testutil.NewMIPSEVM(c.Contracts)
				evm.SetTracer(tracer)
				evm.SetLocalOracle(oracle)
				testutil.LogStepFailureAtCleanup(t, evm)

				fn := path.Join("open_mips_tests/test/bin", f.Name())
				programMem, err := os.ReadFile(fn)
				require.NoError(t, err)

				goVm := c.VMFactory(oracle, os.Stdout, os.Stderr, testutil.CreateLogger())
				state := goVm.GetState()
				err = state.GetMemory().SetMemoryRange(0, bytes.NewReader(programMem))
				require.NoError(t, err, "load program into state")

				// set the return address ($ra) to jump into when test completes
				state.GetRegistersRef()[31] = testutil.EndAddr

				// Catch panics and check if they are expected
				defer func() {
					if r := recover(); r != nil {
						if expectPanic {
							// Success
						} else {
							t.Errorf("unexpected panic: %v", r)
						}
					}
				}()

				for i := 0; i < 1000; i++ {
					curStep := goVm.GetState().GetStep()
					if goVm.GetState().GetPC() == testutil.EndAddr {
						break
					}
					if exitGroup && goVm.GetState().GetExited() {
						break
					}
					insn := state.GetMemory().GetMemory(state.GetPC())
					t.Logf("step: %4d pc: 0x%08x insn: 0x%08x", state.GetStep(), state.GetPC(), insn)

					stepWitness, err := goVm.Step(true)
					require.NoError(t, err)
					evmPost := evm.Step(t, stepWitness, curStep, c.StateHashFn)
					// verify the post-state matches.
					// TODO: maybe more readable to decode the evmPost state, and do attribute-wise comparison.
					goPost, _ := goVm.GetState().EncodeWitness()
					require.Equalf(t, hexutil.Bytes(goPost).String(), hexutil.Bytes(evmPost).String(),
						"mipsevm produced different state than EVM at step %d", state.GetStep())
				}
				if exitGroup {
					require.NotEqual(t, uint32(testutil.EndAddr), goVm.GetState().GetPC(), "must not reach end")
					require.True(t, goVm.GetState().GetExited(), "must set exited state")
					require.Equal(t, uint8(1), goVm.GetState().GetExitCode(), "must exit with 1")
				} else if expectPanic {
					require.NotEqual(t, uint32(testutil.EndAddr), state.GetPC(), "must not reach end")
				} else {
					require.Equal(t, uint32(testutil.EndAddr), state.GetPC(), "must reach end")
					// inspect test result
					done, result := state.GetMemory().GetMemory(testutil.BaseAddrEnd+4), state.GetMemory().GetMemory(testutil.BaseAddrEnd+8)
					require.Equal(t, done, uint32(1), "must be done")
					require.Equal(t, result, uint32(1), "must have success result")
				}
			})
		}
	}
}

func TestEVMSingleStep(t *testing.T) {
	var tracer *tracing.Hooks

	versions := GetMipsVersionTestCases(t)
	cases := []struct {
		name   string
		pc     uint32
		nextPC uint32
		insn   uint32
	}{
		{"j MSB set target", 0, 4, 0x0A_00_00_02},                         // j 0x02_00_00_02
		{"j non-zero PC region", 0x10000000, 0x10000004, 0x08_00_00_02},   // j 0x2
		{"jal MSB set target", 0, 4, 0x0E_00_00_02},                       // jal 0x02_00_00_02
		{"jal non-zero PC region", 0x10000000, 0x10000004, 0x0C_00_00_02}, // jal 0x2
	}

	for _, v := range versions {
		for _, tt := range cases {
			testName := fmt.Sprintf("%v (%v)", tt.name, v.Name)
			t.Run(testName, func(t *testing.T) {
				goVm := v.VMFactory(nil, os.Stdout, os.Stderr, testutil.CreateLogger(), WithPC(tt.pc), WithNextPC(tt.nextPC))
				state := goVm.GetState()
				state.GetMemory().SetMemory(tt.pc, tt.insn)
				curStep := state.GetStep()

				stepWitness, err := goVm.Step(true)
				require.NoError(t, err)

				evm := testutil.NewMIPSEVM(v.Contracts)
				evm.SetTracer(tracer)
				testutil.LogStepFailureAtCleanup(t, evm)

				evmPost := evm.Step(t, stepWitness, curStep, v.StateHashFn)
				goPost, _ := goVm.GetState().EncodeWitness()
				require.Equal(t, hexutil.Bytes(goPost).String(), hexutil.Bytes(evmPost).String(),
					"mipsevm produced different state than EVM")
			})
		}
	}
}

func TestEVM_MMap(t *testing.T) {
	var tracer *tracing.Hooks

	versions := GetMipsVersionTestCases(t)
	cases := []struct {
		name         string
		heap         uint32
		address      uint32
		size         uint32
		shouldFail   bool
		expectedHeap uint32
	}{
		{name: "Increment heap by max value", heap: program.HEAP_START, address: 0, size: ^uint32(0), shouldFail: true},
		{name: "Increment heap to 0", heap: program.HEAP_START, address: 0, size: ^uint32(0) - program.HEAP_START + 1, shouldFail: true},
		{name: "Increment heap to previous page", heap: program.HEAP_START, address: 0, size: ^uint32(0) - program.HEAP_START - memory.PageSize + 1, shouldFail: true},
		{name: "Increment max page size", heap: program.HEAP_START, address: 0, size: ^uint32(0) & ^uint32(memory.PageAddrMask), shouldFail: true},
		{name: "Increment max page size from 0", heap: 0, address: 0, size: ^uint32(0) & ^uint32(memory.PageAddrMask), shouldFail: true},
		{name: "Increment heap at limit", heap: program.HEAP_END, address: 0, size: 1, shouldFail: true},
		{name: "Increment heap to limit", heap: program.HEAP_END - memory.PageSize, address: 0, size: 1, shouldFail: false, expectedHeap: program.HEAP_END},
		{name: "Increment heap within limit", heap: program.HEAP_END - 2*memory.PageSize, address: 0, size: 1, shouldFail: false, expectedHeap: program.HEAP_END - memory.PageSize},
		{name: "Request specific address", heap: program.HEAP_START, address: 0x50_00_00_00, size: 0, shouldFail: false, expectedHeap: program.HEAP_START},
	}

	for _, v := range versions {
		for _, c := range cases {
			testName := fmt.Sprintf("%v (%v)", c.name, v.Name)
			t.Run(testName, func(t *testing.T) {
				goVm := v.VMFactory(nil, os.Stdout, os.Stderr, testutil.CreateLogger(), WithHeap(c.heap))
				state := goVm.GetState()

				state.GetMemory().SetMemory(state.GetPC(), syscallInsn)
				*state.GetRegistersRef() = testutil.RandomRegisters(77)
				state.GetRegistersRef()[2] = exec.SysMmap
				state.GetRegistersRef()[4] = c.address
				state.GetRegistersRef()[5] = c.size
				step := state.GetStep()

				expectedRegisters := testutil.CopyRegisters(state)
				expectedHeap := state.GetHeap()
				expectedMemoryRoot := state.GetMemory().MerkleRoot()
				if c.shouldFail {
					expectedRegisters[2] = exec.SysErrorSignal
					expectedRegisters[7] = exec.MipsEINVAL
				} else {
					expectedHeap = c.expectedHeap
					if c.address == 0 {
						expectedRegisters[2] = state.GetHeap()
						expectedRegisters[7] = 0
					} else {
						expectedRegisters[2] = c.address
						expectedRegisters[7] = 0
					}
				}

				stepWitness, err := goVm.Step(true)
				require.NoError(t, err)

				// Check expectations
				require.Equal(t, step+1, state.GetStep())
				require.Equal(t, expectedHeap, state.GetHeap())
				require.Equal(t, expectedRegisters, state.GetRegistersRef())
				require.Equal(t, expectedMemoryRoot, state.GetMemory().MerkleRoot())
				require.Equal(t, common.Hash{}, state.GetPreimageKey())
				require.Equal(t, uint32(0), state.GetPreimageOffset())
				require.Equal(t, uint32(4), state.GetCpu().PC)
				require.Equal(t, uint32(8), state.GetCpu().NextPC)
				require.Equal(t, uint32(0), state.GetCpu().HI)
				require.Equal(t, uint32(0), state.GetCpu().LO)
				require.Equal(t, false, state.GetExited())
				require.Equal(t, uint8(0), state.GetExitCode())
				require.Equal(t, hexutil.Bytes(nil), state.GetLastHint())

				evm := testutil.NewMIPSEVM(v.Contracts)
				evm.SetTracer(tracer)
				testutil.LogStepFailureAtCleanup(t, evm)

				evmPost := evm.Step(t, stepWitness, step, v.StateHashFn)
				goPost, _ := goVm.GetState().EncodeWitness()
				require.Equal(t, hexutil.Bytes(goPost).String(), hexutil.Bytes(evmPost).String(),
					"mipsevm produced different state than EVM")
			})
		}
	}
}

func TestEVMSysWriteHint(t *testing.T) {
	var tracer *tracing.Hooks

	versions := GetMipsVersionTestCases(t)
	cases := []struct {
		name          string
		memOffset     int      // Where the hint data is stored in memory
		hintData      []byte   // Hint data stored in memory at memOffset
		bytesToWrite  int      // How many bytes of hintData to write
		lastHint      []byte   // The buffer that stores lastHint in the state
		expectedHints [][]byte // The hints we expect to be processed
	}{
		{
			name:      "write 1 full hint at beginning of page",
			memOffset: 4096,
			hintData: []byte{
				0, 0, 0, 6, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite: 10,
			lastHint:     nil,
			expectedHints: [][]byte{
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB},
			},
		},
		{
			name:      "write 1 full hint across page boundary",
			memOffset: 4092,
			hintData: []byte{
				0, 0, 0, 8, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite: 12,
			lastHint:     nil,
			expectedHints: [][]byte{
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB},
			},
		},
		{
			name:      "write 2 full hints",
			memOffset: 5012,
			hintData: []byte{
				0, 0, 0, 6, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, // Hint data
				0, 0, 0, 8, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite: 22,
			lastHint:     nil,
			expectedHints: [][]byte{
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB},
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB},
			},
		},
		{
			name:      "write a single partial hint",
			memOffset: 4092,
			hintData: []byte{
				0, 0, 0, 6, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite:  8,
			lastHint:      nil,
			expectedHints: nil,
		},
		{
			name:      "write 1 full, 1 partial hint",
			memOffset: 5012,
			hintData: []byte{
				0, 0, 0, 6, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, // Hint data
				0, 0, 0, 8, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite: 16,
			lastHint:     nil,
			expectedHints: [][]byte{
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB},
			},
		},
		{
			name:      "write a single partial hint to large capacity lastHint buffer",
			memOffset: 4092,
			hintData: []byte{
				0, 0, 0, 6, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite:  8,
			lastHint:      make([]byte, 0, 4096),
			expectedHints: nil,
		},
		{
			name:      "write full hint to large capacity lastHint buffer",
			memOffset: 5012,
			hintData: []byte{
				0, 0, 0, 6, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite: 10,
			lastHint:     make([]byte, 0, 4096),
			expectedHints: [][]byte{
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB},
			},
		},
		{
			name:      "write multiple hints to large capacity lastHint buffer",
			memOffset: 4092,
			hintData: []byte{
				0, 0, 0, 8, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xCC, 0xCC, // Hint data
				0, 0, 0, 8, // Length prefix
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB, // Hint data
			},
			bytesToWrite: 24,
			lastHint:     make([]byte, 0, 4096),
			expectedHints: [][]byte{
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xCC, 0xCC},
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xBB, 0xBB},
			},
		},
		{
			name:      "write remaining hint data to non-empty lastHint buffer",
			memOffset: 4092,
			hintData: []byte{
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xCC, 0xCC, // Hint data
			},
			bytesToWrite: 8,
			lastHint:     []byte{0, 0, 0, 8},
			expectedHints: [][]byte{
				{0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xCC, 0xCC},
			},
		},
		{
			name:      "write partial hint data to non-empty lastHint buffer",
			memOffset: 4092,
			hintData: []byte{
				0xAA, 0xAA, 0xAA, 0xAA, 0xBB, 0xBB, 0xCC, 0xCC, // Hint data
			},
			bytesToWrite:  4,
			lastHint:      []byte{0, 0, 0, 8},
			expectedHints: nil,
		},
	}

	const (
		insn = uint32(0x00_00_00_0C) // syscall instruction
	)

	for _, v := range versions {
		for _, tt := range cases {
			testName := fmt.Sprintf("%v (%v)", tt.name, v.Name)
			t.Run(testName, func(t *testing.T) {
				oracle := hintTrackingOracle{}
				goVm := v.VMFactory(&oracle, os.Stdout, os.Stderr, testutil.CreateLogger(), WithLastHint(tt.lastHint))
				state := goVm.GetState()
				state.GetRegistersRef()[2] = exec.SysWrite
				state.GetRegistersRef()[4] = exec.FdHintWrite
				state.GetRegistersRef()[5] = uint32(tt.memOffset)
				state.GetRegistersRef()[6] = uint32(tt.bytesToWrite)

				err := state.GetMemory().SetMemoryRange(uint32(tt.memOffset), bytes.NewReader(tt.hintData))
				require.NoError(t, err)
				state.GetMemory().SetMemory(0, insn)
				curStep := state.GetStep()

				stepWitness, err := goVm.Step(true)
				require.NoError(t, err)
				require.Equal(t, tt.expectedHints, oracle.hints)

				evm := testutil.NewMIPSEVM(v.Contracts)
				evm.SetTracer(tracer)
				testutil.LogStepFailureAtCleanup(t, evm)

				evmPost := evm.Step(t, stepWitness, curStep, v.StateHashFn)
				goPost, _ := goVm.GetState().EncodeWitness()
				require.Equal(t, hexutil.Bytes(goPost).String(), hexutil.Bytes(evmPost).String(),
					"mipsevm produced different state than EVM")
			})
		}
	}
}

func TestEVMFault(t *testing.T) {
	var tracer *tracing.Hooks // no-tracer by default, but see test_util.MarkdownTracer
	sender := common.Address{0x13, 0x37}

	versions := GetMipsVersionTestCases(t)
	cases := []struct {
		name   string
		nextPC uint32
		insn   uint32
	}{
		{"illegal instruction", 0, 0xFF_FF_FF_FF},
		{"branch in delay-slot", 8, 0x11_02_00_03},
		{"jump in delay-slot", 8, 0x0c_00_00_0c},
	}

	for _, v := range versions {
		for _, tt := range cases {
			testName := fmt.Sprintf("%v (%v)", tt.name, v.Name)
			t.Run(testName, func(t *testing.T) {
				env, evmState := testutil.NewEVMEnv(v.Contracts)
				env.Config.Tracer = tracer

				goVm := v.VMFactory(nil, os.Stdout, os.Stderr, testutil.CreateLogger(), WithNextPC(tt.nextPC))
				state := goVm.GetState()
				state.GetMemory().SetMemory(0, tt.insn)
				// set the return address ($ra) to jump into when test completes
				state.GetRegistersRef()[31] = testutil.EndAddr

				require.Panics(t, func() { _, _ = goVm.Step(true) })

				insnProof := state.GetMemory().MerkleProof(0)
				encodedWitness, _ := state.EncodeWitness()
				stepWitness := &mipsevm.StepWitness{
					State:     encodedWitness,
					ProofData: insnProof[:],
				}
				input := testutil.EncodeStepInput(t, stepWitness, mipsevm.LocalContext{}, v.Contracts.Artifacts.MIPS)
				startingGas := uint64(30_000_000)

				_, _, err := env.Call(vm.AccountRef(sender), v.Contracts.Addresses.MIPS, input, startingGas, common.U2560)
				require.EqualValues(t, err, vm.ErrExecutionReverted)
				logs := evmState.Logs()
				require.Equal(t, 0, len(logs))
			})
		}
	}
}

func TestHelloEVM(t *testing.T) {
	var tracer *tracing.Hooks // no-tracer by default, but see test_util.MarkdownTracer
	versions := GetMipsVersionTestCases(t)

	for _, v := range versions {
		t.Run(v.Name, func(t *testing.T) {
			evm := testutil.NewMIPSEVM(v.Contracts)
			evm.SetTracer(tracer)
			testutil.LogStepFailureAtCleanup(t, evm)

			var stdOutBuf, stdErrBuf bytes.Buffer
			elfFile := "../../testdata/example/bin/hello.elf"
			goVm := v.ElfVMFactory(t, elfFile, nil, io.MultiWriter(&stdOutBuf, os.Stdout), io.MultiWriter(&stdErrBuf, os.Stderr), testutil.CreateLogger())
			state := goVm.GetState()

			start := time.Now()
			for i := 0; i < 400_000; i++ {
				curStep := goVm.GetState().GetStep()
				if goVm.GetState().GetExited() {
					break
				}
				insn := state.GetMemory().GetMemory(state.GetPC())
				if i%1000 == 0 { // avoid spamming test logs, we are executing many steps
					t.Logf("step: %4d pc: 0x%08x insn: 0x%08x", state.GetStep(), state.GetPC(), insn)
				}

				stepWitness, err := goVm.Step(true)
				require.NoError(t, err)
				evmPost := evm.Step(t, stepWitness, curStep, v.StateHashFn)
				// verify the post-state matches.
				// TODO: maybe more readable to decode the evmPost state, and do attribute-wise comparison.
				goPost, _ := goVm.GetState().EncodeWitness()
				require.Equal(t, hexutil.Bytes(goPost).String(), hexutil.Bytes(evmPost).String(),
					"mipsevm produced different state than EVM")
			}
			end := time.Now()
			delta := end.Sub(start)
			t.Logf("test took %s, %d instructions, %s per instruction", delta, state.GetStep(), delta/time.Duration(state.GetStep()))

			require.True(t, state.GetExited(), "must complete program")
			require.Equal(t, uint8(0), state.GetExitCode(), "exit with 0")

			require.Equal(t, "hello world!\n", stdOutBuf.String(), "stdout says hello")
			require.Equal(t, "", stdErrBuf.String(), "stderr silent")
		})
	}
}

func TestClaimEVM(t *testing.T) {
	var tracer *tracing.Hooks // no-tracer by default, but see test_util.MarkdownTracer
	versions := GetMipsVersionTestCases(t)

	for _, v := range versions {
		t.Run(v.Name, func(t *testing.T) {
			evm := testutil.NewMIPSEVM(v.Contracts)
			evm.SetTracer(tracer)
			testutil.LogStepFailureAtCleanup(t, evm)

			oracle, expectedStdOut, expectedStdErr := testutil.ClaimTestOracle(t)

			var stdOutBuf, stdErrBuf bytes.Buffer
			elfFile := "../../testdata/example/bin/claim.elf"
			goVm := v.ElfVMFactory(t, elfFile, oracle, io.MultiWriter(&stdOutBuf, os.Stdout), io.MultiWriter(&stdErrBuf, os.Stderr), testutil.CreateLogger())
			state := goVm.GetState()

			for i := 0; i < 2000_000; i++ {
				curStep := goVm.GetState().GetStep()
				if goVm.GetState().GetExited() {
					break
				}

				insn := state.GetMemory().GetMemory(state.GetPC())
				if i%1000 == 0 { // avoid spamming test logs, we are executing many steps
					t.Logf("step: %4d pc: 0x%08x insn: 0x%08x", state.GetStep(), state.GetPC(), insn)
				}

				stepWitness, err := goVm.Step(true)
				require.NoError(t, err)

				evmPost := evm.Step(t, stepWitness, curStep, v.StateHashFn)

				goPost, _ := goVm.GetState().EncodeWitness()
				require.Equal(t, hexutil.Bytes(goPost).String(), hexutil.Bytes(evmPost).String(),
					"mipsevm produced different state than EVM")
			}

			require.True(t, state.GetExited(), "must complete program")
			require.Equal(t, uint8(0), state.GetExitCode(), "exit with 0")

			require.Equal(t, expectedStdOut, stdOutBuf.String(), "stdout")
			require.Equal(t, expectedStdErr, stdErrBuf.String(), "stderr")
		})
	}
}

type hintTrackingOracle struct {
	hints [][]byte
}

func (t *hintTrackingOracle) Hint(v []byte) {
	t.hints = append(t.hints, v)
}

func (t *hintTrackingOracle) GetPreimage(k [32]byte) []byte {
	return nil
}
