package cmd

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var (
	outputFile      string
	resetFoliage    bool
	removeCreatures bool
	removeRocks     bool
	removeDeposits  bool
)

var saveCmd = &cobra.Command{
	Use:   "save",
	Short: "Manage and modify Satisfactory save files",
}

type CleanResult struct {
	CreaturesRemoved  int `json:"creaturesRemoved"`
	RocksRemoved      int `json:"rocksRemoved"`
	DepositsRemoved   int `json:"depositsRemoved"`
	FoliageResetCount int `json:"foliageResetCount"`
	TotalBefore       int `json:"totalBefore"`
	TotalAfter        int `json:"totalAfter"`
}

type Transform struct {
	RotX, RotY, RotZ, RotW float32
	TransX, TransY, TransZ float32
	ScaleX, ScaleY, ScaleZ float32
}

type FPackageFileVersion struct {
	UE4Version int32
	UE5Version int32
}

type FEngineVersion struct {
	Major      uint16
	Minor      uint16
	Patch      uint16
	Changelist uint32
	Branch     string
}

type FCustomVersion struct {
	Guid    [16]byte
	Version int32
}

type FCustomVersionContainer struct {
	Versions []FCustomVersion
}

type FSaveObjectVersionData struct {
	SaveObjectVersionDataVersion int32
	PackageFileVersion           FPackageFileVersion
	LicenceVersion               int32
	EngineVersion                FEngineVersion
	CustomVersionContainer       FCustomVersionContainer
}

type ObjectReference struct {
	LevelName string
	PathName  string
}

type SaveHeader struct {
	SaveHeaderType      int32
	SaveVersion         int32
	BuildVersion        int32
	SaveName            string
	MapName             string
	MapOptions          string
	SessionName         string
	PlayedSeconds       int32
	Timestamp           int64
	Visibility          byte
	EditorObjectVersion int32
	ModMetadata         string
	ModFlags            int32
	SaveIdentifier      string
	IsPartitionedWorld  int32
	MD5Valid            int32
	MD5Hash             [16]byte
	CreativeMode        int32
}

type LevelObject struct {
	HeaderType   int32 // 1 for SaveEntity, 0 for SaveComponent
	TypePath     string
	RootObject   string
	InstanceName string
	Flags        uint32

	// Entity fields
	NeedTransform    int32
	Transform        Transform
	WasPlacedInLevel int32

	// Component fields
	ParentEntityName string

	// Data fields
	SaveCustomVersion                   int32
	ShouldMigrateObjectRefsToPersistent int32
	Payload                             []byte
	HasObjectVersionData                int32
	ObjectVersionData                   *FSaveObjectVersionData
}

type Level struct {
	Name                           string
	WritesDestroyedActorsInTOCBlob bool
	SaveCustomVersion              int32
	DestroyedActorsMap             []byte
	Collectables                   []byte
	ObjectVersionData              *FSaveObjectVersionData
	Objects                        []*LevelObject
}

type SaveBodyValidation struct {
	Raw []byte
}

func readString(r io.Reader) (string, error) {
	var length int32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length == 0 {
		return "", nil
	}
	if length < 0 {
		utf16Len := int(-length) * 2
		buf := make([]byte, utf16Len)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		var sb bytes.Buffer
		for i := 0; i < len(buf); i += 2 {
			sb.WriteByte(buf[i])
		}
		return sb.String(), nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	if len(buf) > 0 && buf[len(buf)-1] == 0 {
		buf = buf[:len(buf)-1]
	}
	return string(buf), nil
}

func writeString(w io.Writer, s string) error {
	if s == "" {
		return binary.Write(w, binary.LittleEndian, int32(0))
	}
	buf := append([]byte(s), 0)
	length := int32(len(buf))
	if err := binary.Write(w, binary.LittleEndian, length); err != nil {
		return err
	}
	_, err := w.Write(buf)
	return err
}

func readVersionData(r io.Reader) (*FSaveObjectVersionData, error) {
	var data FSaveObjectVersionData
	binary.Read(r, binary.LittleEndian, &data.SaveObjectVersionDataVersion)
	binary.Read(r, binary.LittleEndian, &data.PackageFileVersion.UE4Version)
	binary.Read(r, binary.LittleEndian, &data.PackageFileVersion.UE5Version)
	binary.Read(r, binary.LittleEndian, &data.LicenceVersion)
	binary.Read(r, binary.LittleEndian, &data.EngineVersion.Major)
	binary.Read(r, binary.LittleEndian, &data.EngineVersion.Minor)
	binary.Read(r, binary.LittleEndian, &data.EngineVersion.Patch)
	binary.Read(r, binary.LittleEndian, &data.EngineVersion.Changelist)
	branch, _ := readString(r)
	data.EngineVersion.Branch = branch

	var count int32
	binary.Read(r, binary.LittleEndian, &count)
	data.CustomVersionContainer.Versions = make([]FCustomVersion, count)
	for i := 0; i < int(count); i++ {
		var ver FCustomVersion
		io.ReadFull(r, ver.Guid[:])
		binary.Read(r, binary.LittleEndian, &ver.Version)
		data.CustomVersionContainer.Versions[i] = ver
	}
	return &data, nil
}

func writeVersionData(w io.Writer, data *FSaveObjectVersionData) error {
	binary.Write(w, binary.LittleEndian, data.SaveObjectVersionDataVersion)
	binary.Write(w, binary.LittleEndian, data.PackageFileVersion.UE4Version)
	binary.Write(w, binary.LittleEndian, data.PackageFileVersion.UE5Version)
	binary.Write(w, binary.LittleEndian, data.LicenceVersion)
	binary.Write(w, binary.LittleEndian, data.EngineVersion.Major)
	binary.Write(w, binary.LittleEndian, data.EngineVersion.Minor)
	binary.Write(w, binary.LittleEndian, data.EngineVersion.Patch)
	binary.Write(w, binary.LittleEndian, data.EngineVersion.Changelist)
	writeString(w, data.EngineVersion.Branch)

	binary.Write(w, binary.LittleEndian, int32(len(data.CustomVersionContainer.Versions)))
	for _, ver := range data.CustomVersionContainer.Versions {
		w.Write(ver.Guid[:])
		binary.Write(w, binary.LittleEndian, ver.Version)
	}
	return nil
}

func readObjectReference(r io.Reader) (ObjectReference, error) {
	var ref ObjectReference
	var err error
	ref.LevelName, err = readString(r)
	if err != nil {
		return ref, err
	}
	ref.PathName, err = readString(r)
	return ref, err
}

func writeObjectReference(w io.Writer, ref ObjectReference) error {
	if err := writeString(w, ref.LevelName); err != nil {
		return err
	}
	return writeString(w, ref.PathName)
}

func readObjectReferencesListRaw(r io.Reader) ([]byte, error) {
	var count int32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, count)
	for i := 0; i < int(count); i++ {
		level, _ := readString(r)
		path, _ := readString(r)
		writeString(&buf, level)
		writeString(&buf, path)
	}
	return buf.Bytes(), nil
}

func readDestroyedActorsMapRaw(r io.Reader) ([]byte, error) {
	var count int32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, count)
	for i := 0; i < int(count); i++ {
		level, _ := readString(r)
		var numDestroyed int32
		binary.Read(r, binary.LittleEndian, &numDestroyed)
		writeString(&buf, level)
		binary.Write(&buf, binary.LittleEndian, numDestroyed)
		for j := 0; j < int(numDestroyed); j++ {
			path, _ := readString(r)
			writeString(&buf, path)
		}
	}
	return buf.Bytes(), nil
}

func getObjectSaveVersion(saveVersion int32, levelSaveCustomVersion int32, objSaveCustomVersion int32) int32 {
	levelVer := levelSaveCustomVersion
	if levelVer == 0 {
		levelVer = saveVersion
	}
	objVer := objSaveCustomVersion
	if objVer == 0 {
		objVer = levelVer
	}
	return objVer
}

var cleanCmd = &cobra.Command{
	Use:   "clean <save-file>",
	Short: "Remove all creatures, spawners, and optionally reset foliage from a save file",
	RunE: func(cmd *cobra.Command, args []string) error {
		inputPath := args[0]
		if _, err := os.Stat(inputPath); os.IsNotExist(err) {
			return fmt.Errorf("save file %s does not exist", inputPath)
		}

		absInputPath, err := filepath.Abs(inputPath)
		if err != nil {
			return fmt.Errorf("failed to get absolute path for input: %w", err)
		}

		outputPath := outputFile
		if outputPath == "" {
			ext := filepath.Ext(absInputPath)
			base := strings.TrimSuffix(absInputPath, ext)
			outputPath = base + "_clean" + ext
		}

		fmt.Println("Cleaning save file, please wait...")

		result, err := cleanSaveFile(absInputPath, outputPath, resetFoliage, removeCreatures, removeRocks, removeDeposits)
		if err != nil {
			return fmt.Errorf("save clean failed: %w", err)
		}

		fmt.Printf("\nSave cleaning successful:\n")
		fmt.Printf("  - Total objects before: %d\n", result.TotalBefore)
		fmt.Printf("  - Total objects after:  %d\n", result.TotalAfter)
		if removeCreatures {
			fmt.Printf("  - Creatures removed:    %d\n", result.CreaturesRemoved)
		}
		if removeRocks {
			fmt.Printf("  - Destructible rocks removed: %d\n", result.RocksRemoved)
		}
		if removeDeposits {
			fmt.Printf("  - Resource deposits removed:  %d\n", result.DepositsRemoved)
		}
		if resetFoliage {
			fmt.Printf("  - Foliage cells reset:  %d (previously chopped trees/bushes regrown)\n", result.FoliageResetCount)
		}
		fmt.Printf("  - Cleaned save written to: %s\n", outputPath)

		return nil
	},
}

// cleanSaveFile is the native Go implementation for parsing, cleaning, and writing Satisfactory saves.
func cleanSaveFile(inputPath string, outputPath string, resetFoliage, removeCreatures, removeRocks, removeDeposits bool) (CleanResult, error) {
	var result CleanResult
	file, err := os.Open(inputPath)
	if err != nil {
		return CleanResult{}, err
	}
	defer file.Close()

	var save SaveHeader
	if err := binary.Read(file, binary.LittleEndian, &save.SaveHeaderType); err != nil {
		return CleanResult{}, err
	}
	binary.Read(file, binary.LittleEndian, &save.SaveVersion)
	binary.Read(file, binary.LittleEndian, &save.BuildVersion)
	save.SaveName, _ = readString(file)
	save.MapName, _ = readString(file)
	save.MapOptions, _ = readString(file)
	save.SessionName, _ = readString(file)
	binary.Read(file, binary.LittleEndian, &save.PlayedSeconds)
	binary.Read(file, binary.LittleEndian, &save.Timestamp)
	binary.Read(file, binary.LittleEndian, &save.Visibility)
	binary.Read(file, binary.LittleEndian, &save.EditorObjectVersion)
	save.ModMetadata, _ = readString(file)
	binary.Read(file, binary.LittleEndian, &save.ModFlags)
	save.SaveIdentifier, _ = readString(file)
	binary.Read(file, binary.LittleEndian, &save.IsPartitionedWorld)
	binary.Read(file, binary.LittleEndian, &save.MD5Valid)
	io.ReadFull(file, save.MD5Hash[:])
	binary.Read(file, binary.LittleEndian, &save.CreativeMode)

	// Decompress all chunks
	var bodyBuffer bytes.Buffer
	for {
		var magic uint32
		err := binary.Read(file, binary.LittleEndian, &magic)
		if err == io.EOF {
			break
		}
		if magic != 0x9E2A83C1 {
			break
		}
		var always22 uint32
		binary.Read(file, binary.LittleEndian, &always22)
		var maxChunkSize uint32
		binary.Read(file, binary.LittleEndian, &maxChunkSize)
		var zero byte
		binary.Read(file, binary.LittleEndian, &zero)
		var always03 uint32
		binary.Read(file, binary.LittleEndian, &always03)
		var compSize int64
		binary.Read(file, binary.LittleEndian, &compSize)
		var uncompSize int64
		binary.Read(file, binary.LittleEndian, &uncompSize)
		var compSize2 int64
		binary.Read(file, binary.LittleEndian, &compSize2)
		var uncompSize2 int64
		binary.Read(file, binary.LittleEndian, &uncompSize2)

		compBytes := make([]byte, compSize)
		if _, err := io.ReadFull(file, compBytes); err != nil {
			return CleanResult{}, err
		}

		zReader, err := zlib.NewReader(bytes.NewReader(compBytes))
		if err != nil {
			return CleanResult{}, err
		}
		uncompBytes := make([]byte, uncompSize)
		io.ReadFull(zReader, uncompBytes)
		zReader.Close()

		bodyBuffer.Write(uncompBytes)
	}

	r := bytes.NewReader(bodyBuffer.Bytes())

	var uncompressedSizeField int64
	binary.Read(r, binary.LittleEndian, &uncompressedSizeField)

	var objVerData *FSaveObjectVersionData
	if save.SaveVersion >= 53 {
		objVerData, _ = readVersionData(r)
	}

	var bodyValidation SaveBodyValidation
	if save.SaveVersion >= 38 {
		var gridCount int32
		binary.Read(r, binary.LittleEndian, &gridCount)
		var gridBuf bytes.Buffer
		binary.Write(&gridBuf, binary.LittleEndian, gridCount)
		for i := 0; i < int(gridCount); i++ {
			gridName, _ := readString(r)
			writeString(&gridBuf, gridName)
			var cellSize int32
			binary.Read(r, binary.LittleEndian, &cellSize)
			binary.Write(&gridBuf, binary.LittleEndian, cellSize)
			var gridHash uint32
			binary.Read(r, binary.LittleEndian, &gridHash)
			binary.Write(&gridBuf, binary.LittleEndian, gridHash)

			var childrenCount uint32
			binary.Read(r, binary.LittleEndian, &childrenCount)
			binary.Write(&gridBuf, binary.LittleEndian, childrenCount)
			for j := 0; j < int(childrenCount); j++ {
				levelName, _ := readString(r)
				writeString(&gridBuf, levelName)
				var cellHash uint32
				binary.Read(r, binary.LittleEndian, &cellHash)
				binary.Write(&gridBuf, binary.LittleEndian, cellHash)
			}
		}
		bodyValidation.Raw = gridBuf.Bytes()
	}

	var levelCount int32
	binary.Read(r, binary.LittleEndian, &levelCount)

	levels := make([]*Level, levelCount+1)
	for i := 0; i <= int(levelCount); i++ {
		var name string
		if i == int(levelCount) {
			name = save.MapName
		} else {
			name, _ = readString(r)
		}

		level := &Level{Name: name}

		var tocLength int32
		binary.Read(r, binary.LittleEndian, &tocLength)
		if save.SaveVersion >= 37 {
			var z int32
			binary.Read(r, binary.LittleEndian, &z)
		}
		tocBlobPos, _ := r.Seek(0, io.SeekCurrent)
		r.Seek(int64(tocLength), io.SeekCurrent)

		var dataLength int32
		binary.Read(r, binary.LittleEndian, &dataLength)
		if save.SaveVersion >= 37 {
			var z int32
			binary.Read(r, binary.LittleEndian, &z)
		}
		dataBlobPos, _ := r.Seek(0, io.SeekCurrent)
		r.Seek(int64(dataLength), io.SeekCurrent)

		isPersistent := name == save.MapName
		if !isPersistent {
			if save.SaveVersion >= 51 {
				binary.Read(r, binary.LittleEndian, &level.SaveCustomVersion)
			}
		}

		if isPersistent {
			level.DestroyedActorsMap, _ = readDestroyedActorsMapRaw(r)
		} else {
			level.Collectables, _ = readObjectReferencesListRaw(r)
			if save.SaveVersion >= 53 {
				var shouldSerialize int32
				binary.Read(r, binary.LittleEndian, &shouldSerialize)
				if shouldSerialize > 0 {
					level.ObjectVersionData, _ = readVersionData(r)
				}
			}
		}

		endOfLevelPos, _ := r.Seek(0, io.SeekCurrent)

		r.Seek(tocBlobPos, io.SeekStart)
		var headerCount int32
		binary.Read(r, binary.LittleEndian, &headerCount)
		level.Objects = make([]*LevelObject, headerCount)
		for j := 0; j < int(headerCount); j++ {
			obj := &LevelObject{}
			binary.Read(r, binary.LittleEndian, &obj.HeaderType)
			obj.TypePath, _ = readString(r)
			obj.RootObject, _ = readString(r)
			obj.InstanceName, _ = readString(r)
			if save.SaveVersion >= 49 {
				binary.Read(r, binary.LittleEndian, &obj.Flags)
			}

			if obj.HeaderType == 1 {
				binary.Read(r, binary.LittleEndian, &obj.NeedTransform)
				binary.Read(r, binary.LittleEndian, &obj.Transform.RotX)
				binary.Read(r, binary.LittleEndian, &obj.Transform.RotY)
				binary.Read(r, binary.LittleEndian, &obj.Transform.RotZ)
				binary.Read(r, binary.LittleEndian, &obj.Transform.RotW)
				binary.Read(r, binary.LittleEndian, &obj.Transform.TransX)
				binary.Read(r, binary.LittleEndian, &obj.Transform.TransY)
				binary.Read(r, binary.LittleEndian, &obj.Transform.TransZ)
				binary.Read(r, binary.LittleEndian, &obj.Transform.ScaleX)
				binary.Read(r, binary.LittleEndian, &obj.Transform.ScaleY)
				binary.Read(r, binary.LittleEndian, &obj.Transform.ScaleZ)
				binary.Read(r, binary.LittleEndian, &obj.WasPlacedInLevel)
			} else {
				obj.ParentEntityName, _ = readString(r)
			}
			level.Objects[j] = obj
		}

		currentTOCPos, _ := r.Seek(0, io.SeekCurrent)
		remainingTOCBytes := int32(tocBlobPos + int64(tocLength) - currentTOCPos)
		if remainingTOCBytes > 0 {
			level.WritesDestroyedActorsInTOCBlob = true
			if isPersistent {
				level.DestroyedActorsMap, _ = readDestroyedActorsMapRaw(r)
			} else {
				level.Collectables, _ = readObjectReferencesListRaw(r)
			}
		}

		r.Seek(dataBlobPos, io.SeekStart)
		var entityCount int32
		binary.Read(r, binary.LittleEndian, &entityCount)

		for j := 0; j < int(entityCount); j++ {
			obj := level.Objects[j]
			if save.SaveVersion >= 38 {
				binary.Read(r, binary.LittleEndian, &obj.SaveCustomVersion)
				binary.Read(r, binary.LittleEndian, &obj.ShouldMigrateObjectRefsToPersistent)
			}
			var binarySize int32
			binary.Read(r, binary.LittleEndian, &binarySize)
			obj.Payload = make([]byte, binarySize)
			io.ReadFull(r, obj.Payload)

			objVer := getObjectSaveVersion(save.SaveVersion, level.SaveCustomVersion, obj.SaveCustomVersion)
			if objVer >= 53 {
				binary.Read(r, binary.LittleEndian, &obj.HasObjectVersionData)
				if obj.HasObjectVersionData > 0 {
					obj.ObjectVersionData, _ = readVersionData(r)
				}
			}
		}

		r.Seek(endOfLevelPos, io.SeekStart)
		levels[i] = level
	}

	var refCount int32
	unresolvedWorldSaveData := []ObjectReference{}
	if r.Len() >= 4 {
		binary.Read(r, binary.LittleEndian, &refCount)
		unresolvedWorldSaveData = make([]ObjectReference, refCount)
		for i := 0; i < int(refCount); i++ {
			unresolvedWorldSaveData[i], _ = readObjectReference(r)
		}
	}

	// Filter creatures, spawners, rocks, and deposits
	var removedCount int
	var rocksRemovedCount int
	var depositsRemovedCount int
	var totalBefore int
	var totalAfter int
	var foliageResetCount int

	for _, lvl := range levels {
		totalBefore += len(lvl.Objects)
		filtered := []*LevelObject{}
		for _, obj := range lvl.Objects {
			shouldRemove := false
			if removeCreatures && strings.Contains(strings.ToLower(obj.TypePath), "/creature/") {
				shouldRemove = true
				removedCount++
			} else if removeRocks && (strings.Contains(obj.TypePath, "BP_DestructibleFlatRock") ||
				strings.Contains(obj.TypePath, "BP_DestructibleLargeRock") ||
				strings.Contains(obj.TypePath, "BP_DestructibleSmallRock")) {
				shouldRemove = true
				rocksRemovedCount++
			} else if removeDeposits && strings.Contains(obj.TypePath, "BP_ResourceDeposit") {
				shouldRemove = true
				depositsRemovedCount++
			}

			if shouldRemove {
				// skip
			} else {
				if resetFoliage && strings.Contains(obj.TypePath, "FGFoliageRemovalSubsystem") {
					payloadBytes, err := hex.DecodeString("00000000000000000000000000160000006d5361766564466f6c696167654772696453697a65000f00000055496e74333250726f706572747900000000000400000000001900000a0000006d5361766544617461000c0000004d617050726f706572747900020000000f00000053747275637450726f706572747900010000000a000000496e74566563746f720001000000140000002f5363726970742f436f7265554f626a65637400000000000f00000053747275637450726f706572747900010000001e000000466f6c6961676552656d6f76616c536176654461746150657243656c6c0001000000140000002f5363726970742f466163746f727947616d65000000000008000000080000000000000000050000004e6f6e650000000000")
					if err == nil {
						origLen := len(obj.Payload)
						obj.Payload = payloadBytes
						foliageResetCount = (origLen - 289) / 3500
						if foliageResetCount < 0 {
							foliageResetCount = 0
						} else {
							foliageResetCount += 1
						}
					}
				}
				filtered = append(filtered, obj)
			}
		}
		lvl.Objects = filtered
		totalAfter += len(lvl.Objects)
	}

	// Serialize back
	var wBuf bytes.Buffer
	binary.Write(&wBuf, binary.LittleEndian, int64(0))

	if save.SaveVersion >= 53 {
		writeVersionData(&wBuf, objVerData)
	}

	if save.SaveVersion >= 38 {
		wBuf.Write(bodyValidation.Raw)
	}

	binary.Write(&wBuf, binary.LittleEndian, int32(len(levels)-1))
	for i, lvl := range levels {
		if i < int(levelCount) {
			writeString(&wBuf, lvl.Name)
		}

		tocSizePlaceholderPos := wBuf.Len()
		binary.Write(&wBuf, binary.LittleEndian, int32(0))
		if save.SaveVersion >= 37 {
			binary.Write(&wBuf, binary.LittleEndian, int32(0))
		}
		tocBlobStartPos := wBuf.Len()

		binary.Write(&wBuf, binary.LittleEndian, int32(len(lvl.Objects)))
		for _, obj := range lvl.Objects {
			binary.Write(&wBuf, binary.LittleEndian, obj.HeaderType)
			writeString(&wBuf, obj.TypePath)
			writeString(&wBuf, obj.RootObject)
			writeString(&wBuf, obj.InstanceName)
			if save.SaveVersion >= 49 {
				binary.Write(&wBuf, binary.LittleEndian, obj.Flags)
			}
			if obj.HeaderType == 1 {
				binary.Write(&wBuf, binary.LittleEndian, obj.NeedTransform)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.RotX)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.RotY)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.RotZ)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.RotW)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.TransX)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.TransY)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.TransZ)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.ScaleX)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.ScaleY)
				binary.Write(&wBuf, binary.LittleEndian, obj.Transform.ScaleZ)
				binary.Write(&wBuf, binary.LittleEndian, obj.WasPlacedInLevel)
			} else {
				writeString(&wBuf, obj.ParentEntityName)
			}
		}

		if lvl.WritesDestroyedActorsInTOCBlob {
			isPersistent := lvl.Name == save.MapName
			if isPersistent {
				wBuf.Write(lvl.DestroyedActorsMap)
			} else {
				wBuf.Write(lvl.Collectables)
			}
		}

		tocBlobEndPos := wBuf.Len()
		tocLength := int32(tocBlobEndPos - tocBlobStartPos)
		tocBytes := wBuf.Bytes()
		binary.LittleEndian.PutUint32(tocBytes[tocSizePlaceholderPos:], uint32(tocLength))

		dataSizePlaceholderPos := wBuf.Len()
		binary.Write(&wBuf, binary.LittleEndian, int32(0))
		if save.SaveVersion >= 37 {
			binary.Write(&wBuf, binary.LittleEndian, int32(0))
		}
		dataBlobStartPos := wBuf.Len()

		binary.Write(&wBuf, binary.LittleEndian, int32(len(lvl.Objects)))
		for _, obj := range lvl.Objects {
			if save.SaveVersion >= 38 {
				binary.Write(&wBuf, binary.LittleEndian, obj.SaveCustomVersion)
				binary.Write(&wBuf, binary.LittleEndian, obj.ShouldMigrateObjectRefsToPersistent)
			}
			binary.Write(&wBuf, binary.LittleEndian, int32(len(obj.Payload)))
			wBuf.Write(obj.Payload)

			objVer := getObjectSaveVersion(save.SaveVersion, lvl.SaveCustomVersion, obj.SaveCustomVersion)
			if objVer >= 53 {
				binary.Write(&wBuf, binary.LittleEndian, obj.HasObjectVersionData)
				if obj.HasObjectVersionData > 0 {
					writeVersionData(&wBuf, obj.ObjectVersionData)
				}
			}
		}

		dataBlobEndPos := wBuf.Len()
		dataLength := int32(dataBlobEndPos - dataBlobStartPos)
		dataBytes := wBuf.Bytes()
		binary.LittleEndian.PutUint32(dataBytes[dataSizePlaceholderPos:], uint32(dataLength))

		isPersistent := lvl.Name == save.MapName
		if !isPersistent {
			if save.SaveVersion >= 51 {
				binary.Write(&wBuf, binary.LittleEndian, lvl.SaveCustomVersion)
			}
		}
		if isPersistent {
			wBuf.Write(lvl.DestroyedActorsMap)
		} else {
			wBuf.Write(lvl.Collectables)
			if save.SaveVersion >= 53 {
				if lvl.ObjectVersionData != nil {
					binary.Write(&wBuf, binary.LittleEndian, int32(1))
					writeVersionData(&wBuf, lvl.ObjectVersionData)
				} else {
					binary.Write(&wBuf, binary.LittleEndian, int32(0))
				}
			}
		}
	}

	binary.Write(&wBuf, binary.LittleEndian, int32(len(unresolvedWorldSaveData)))
	for _, ref := range unresolvedWorldSaveData {
		writeObjectReference(&wBuf, ref)
	}

	totalBodyBytes := wBuf.Bytes()
	uncompBodySize := int64(len(totalBodyBytes) - 8)
	binary.LittleEndian.PutUint64(totalBodyBytes[0:], uint64(uncompBodySize))

	var compBuf bytes.Buffer

	var headerBuf bytes.Buffer
	binary.Write(&headerBuf, binary.LittleEndian, save.SaveHeaderType)
	binary.Write(&headerBuf, binary.LittleEndian, save.SaveVersion)
	binary.Write(&headerBuf, binary.LittleEndian, save.BuildVersion)
	writeString(&headerBuf, save.SaveName)
	writeString(&headerBuf, save.MapName)
	writeString(&headerBuf, save.MapOptions)
	writeString(&headerBuf, save.SessionName)
	binary.Write(&headerBuf, binary.LittleEndian, save.PlayedSeconds)
	binary.Write(&headerBuf, binary.LittleEndian, save.Timestamp)
	binary.Write(&headerBuf, binary.LittleEndian, save.Visibility)
	binary.Write(&headerBuf, binary.LittleEndian, save.EditorObjectVersion)
	writeString(&headerBuf, save.ModMetadata)
	binary.Write(&headerBuf, binary.LittleEndian, save.ModFlags)
	writeString(&headerBuf, save.SaveIdentifier)
	binary.Write(&headerBuf, binary.LittleEndian, save.IsPartitionedWorld)
	binary.Write(&headerBuf, binary.LittleEndian, save.MD5Valid)
	
	md5Pos := headerBuf.Len()
	headerBuf.Write(make([]byte, 16))
	binary.Write(&headerBuf, binary.LittleEndian, save.CreativeMode)

	chunkSize := 131072
	for offset := 0; offset < len(totalBodyBytes); offset += chunkSize {
		end := offset + chunkSize
		if end > len(totalBodyBytes) {
			end = len(totalBodyBytes)
		}
		rawChunk := totalBodyBytes[offset:end]

		var zBuf bytes.Buffer
		zWriter := zlib.NewWriter(&zBuf)
		zWriter.Write(rawChunk)
		zWriter.Close()

		compChunkBytes := zBuf.Bytes()

		binary.Write(&compBuf, binary.LittleEndian, uint32(0x9E2A83C1))
		binary.Write(&compBuf, binary.LittleEndian, uint32(0x22222222))
		binary.Write(&compBuf, binary.LittleEndian, uint32(131072))
		binary.Write(&compBuf, binary.LittleEndian, byte(0))
		binary.Write(&compBuf, binary.LittleEndian, uint32(0x03000000))
		binary.Write(&compBuf, binary.LittleEndian, int64(len(compChunkBytes)))
		binary.Write(&compBuf, binary.LittleEndian, int64(len(rawChunk)))
		binary.Write(&compBuf, binary.LittleEndian, int64(len(compChunkBytes)))
		binary.Write(&compBuf, binary.LittleEndian, int64(len(rawChunk)))
		compBuf.Write(compChunkBytes)
	}

	bodyMd5 := md5.Sum(compBuf.Bytes())
	headerBytes := headerBuf.Bytes()
	copy(headerBytes[md5Pos:], bodyMd5[:])

	outFile, err := os.Create(outputPath)
	if err != nil {
		return CleanResult{}, err
	}
	defer outFile.Close()

	if _, err := outFile.Write(headerBytes); err != nil {
		return CleanResult{}, err
	}
	if _, err := outFile.Write(compBuf.Bytes()); err != nil {
		return CleanResult{}, err
	}

	result.TotalBefore = totalBefore
	result.TotalAfter = totalAfter
	result.CreaturesRemoved = removedCount
	result.RocksRemoved = rocksRemovedCount
	result.DepositsRemoved = depositsRemovedCount
	result.FoliageResetCount = foliageResetCount

	return result, nil
}

func init() {
	cleanCmd.Flags().StringVarP(&outputFile, "output", "o", "", "Path to write the cleaned save file (default: <input-file>_clean.sav)")
	cleanCmd.Flags().BoolVar(&resetFoliage, "reset-foliage", false, "Reset all removed foliage (make chopped trees/bushes regrow)")
	cleanCmd.Flags().BoolVar(&removeCreatures, "remove-creatures", true, "Remove all creatures/critters")
	cleanCmd.Flags().BoolVar(&removeRocks, "remove-rocks", false, "Remove all destructible cracked boulders")
	cleanCmd.Flags().BoolVar(&removeDeposits, "remove-deposits", false, "Remove all temporary resource node deposits")

	saveCmd.AddCommand(cleanCmd)
}
