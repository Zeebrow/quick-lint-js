// Copyright (C) 2020  Matthew "strager" Glazar
// See end of file for extended copyright information.

package main

import "archive/tar"
import "archive/zip"
import "bytes"
import "compress/gzip"
import "crypto/sha1"
import "crypto/sha256"
import "encoding/hex"
import "errors"
import "flag"
import "fmt"
import "io"
import "io/fs"
import "io/ioutil"
import "log"
import "os"
import "os/exec"
import "path/filepath"
import "strings"
import "time"
import _ "embed"

//go:embed certificates/quick-lint-js.cer
var AppleCodesignCertificate []byte

//go:embed certificates/quick-lint-js.gpg.key
var QLJSGPGKey []byte

type SigningStuff struct {
	AppleCodesignIdentity string // Common Name from the macOS Keychain.
	Certificate           []byte
	CertificateSHA1Hash   [20]byte
	GPGIdentity           string // Fingerprint or email or name.
	GPGKey                []byte
	PrivateKeyPKCS12Path  string
}

// Key: SHA256 hash of original file
// Value: contents of transformed (signed) file
var TransformCache map[SHA256Hash]FileTransformResult = make(map[SHA256Hash]FileTransformResult)

var signingStuff SigningStuff

var ProgramStartTime time.Time = time.Now()
var TempDirs []string

func main() {
	defer RemoveTempDirs()

	signingStuff.Certificate = AppleCodesignCertificate
	signingStuff.GPGKey = QLJSGPGKey

	flag.StringVar(&signingStuff.AppleCodesignIdentity, "AppleCodesignIdentity", "", "")
	flag.StringVar(&signingStuff.GPGIdentity, "GPGIdentity", "", "")
	flag.StringVar(&signingStuff.PrivateKeyPKCS12Path, "PrivateKeyPKCS12", "", "")
	flag.Parse()
	if flag.NArg() != 2 {
		os.Stderr.WriteString(fmt.Sprintf("error: source and destination directories\n"))
		os.Exit(2)
	}

	signingStuff.CertificateSHA1Hash = sha1.Sum(AppleCodesignCertificate)

	sourceDir := flag.Args()[0]
	destinationDir := flag.Args()[1]

	hashes := ListOfHashes{}
	err := filepath.Walk(sourceDir, func(sourcePath string, sourceInfo fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relativePath, err := filepath.Rel(sourceDir, sourcePath)
		if err != nil {
			log.Fatal(err)
		}

		destinationPath := filepath.Join(destinationDir, relativePath)
		if sourceInfo.IsDir() {
			err = os.Mkdir(destinationPath, sourceInfo.Mode())
			if err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		} else {
			err = CopyFileOrTransformArchive(NewDeepPath(relativePath), sourcePath, destinationPath, sourceInfo)
			if err != nil {
				return err
			}

			if err := hashes.AddHashOfFile(destinationPath, relativePath); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := CheckUnsignedFiles(); err != nil {
		log.Fatal(err)
	}
	if err := CheckDoubleSigning(sourceDir, destinationDir); err != nil {
		log.Fatal(err)
	}

	hashesPath := filepath.Join(destinationDir, "SHA256SUMS")
	if err := hashes.DumpSHA256HashesToFile(hashesPath); err != nil {
		log.Fatal(err)
	}

	log.Printf("signing with GPG: %s\n", hashesPath)
	if _, err := GPGSignFile(hashesPath); err != nil {
		log.Fatal(err)
	}
	if err := GPGVerifySignature(hashesPath, hashesPath+".asc"); err != nil {
		log.Fatal(err)
	}

	if err := VerifySHA256SUMSFile(hashesPath); err != nil {
		log.Fatal(err)
	}

	sourceTarballPath := filepath.Join(destinationDir, "source/quick-lint-js-2.3.0.tar.gz")
	log.Printf("signing with GPG: %s\n", sourceTarballPath)
	if _, err := GPGSignFile(sourceTarballPath); err != nil {
		log.Fatal(err)
	}
	if err := GPGVerifySignature(sourceTarballPath, sourceTarballPath+".asc"); err != nil {
		log.Fatal(err)
	}
}

func RemoveTempDirs() {
	for _, tempDir := range TempDirs {
		os.RemoveAll(tempDir)
	}
}

type FileTransformType int

const (
	NoTransform FileTransformType = iota
	AppleCodesign
	GPGSign
	MicrosoftOsslsigncode
)

var filesToTransform map[DeepPath]FileTransformType = map[DeepPath]FileTransformType{
	NewDeepPath3("chocolatey/quick-lint-js.nupkg", "tools/windows-x64.zip", "bin/quick-lint-js.exe"):              MicrosoftOsslsigncode,
	NewDeepPath3("chocolatey/quick-lint-js.nupkg", "tools/windows-x86.zip", "bin/quick-lint-js.exe"):              MicrosoftOsslsigncode,
	NewDeepPath2("manual/linux-aarch64.tar.gz", "quick-lint-js/bin/quick-lint-js"):                                GPGSign,
	NewDeepPath2("manual/linux-armhf.tar.gz", "quick-lint-js/bin/quick-lint-js"):                                  GPGSign,
	NewDeepPath2("manual/linux.tar.gz", "quick-lint-js/bin/quick-lint-js"):                                        GPGSign,
	NewDeepPath2("manual/macos-aarch64.tar.gz", "quick-lint-js/bin/quick-lint-js"):                                AppleCodesign,
	NewDeepPath2("manual/macos.tar.gz", "quick-lint-js/bin/quick-lint-js"):                                        AppleCodesign,
	NewDeepPath2("manual/windows-arm64.zip", "bin/quick-lint-js.exe"):                                             MicrosoftOsslsigncode,
	NewDeepPath2("manual/windows-arm.zip", "bin/quick-lint-js.exe"):                                               MicrosoftOsslsigncode,
	NewDeepPath2("manual/windows-x86.zip", "bin/quick-lint-js.exe"):                                               MicrosoftOsslsigncode,
	NewDeepPath2("manual/windows.zip", "bin/quick-lint-js.exe"):                                                   MicrosoftOsslsigncode,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/darwin-arm64/bin/quick-lint-js"):                         AppleCodesign,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/darwin-x64/bin/quick-lint-js"):                           AppleCodesign,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/linux-arm/bin/quick-lint-js"):                            GPGSign,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/linux-arm64/bin/quick-lint-js"):                          GPGSign,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/linux-x64/bin/quick-lint-js"):                            GPGSign,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/win32-arm64/bin/quick-lint-js.exe"):                      MicrosoftOsslsigncode,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/win32-ia32/bin/quick-lint-js.exe"):                       MicrosoftOsslsigncode,
	NewDeepPath2("npm/quick-lint-js-2.3.0.tgz", "package/win32-x64/bin/quick-lint-js.exe"):                        MicrosoftOsslsigncode,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_darwin-arm64.node"): AppleCodesign,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_darwin-x64.node"):   AppleCodesign,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_linux-arm.node"):    GPGSign,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_linux-arm64.node"):  GPGSign,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_linux-x64.node"):    GPGSign,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_win32-arm.node"):    MicrosoftOsslsigncode,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_win32-arm64.node"):  MicrosoftOsslsigncode,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_win32-ia32.node"):   MicrosoftOsslsigncode,
	NewDeepPath2("vscode/quick-lint-js-2.3.0.vsix", "extension/dist/quick-lint-js-vscode-node_win32-x64.node"):    MicrosoftOsslsigncode,
}

func CheckUnsignedFiles() error {
	foundError := false
	for deepPath, _ := range filesToTransform {
		log.Printf("file should have been signed but wasn't: %v", deepPath)
		foundError = true
	}
	if foundError {
		return fmt.Errorf("one or more files were not signed")
	}
	return nil
}

// Verify that modified files are modified in an idempotent
// way.
//
// If sourceDir contains a.exe and b.exe, then
// destinationDir should contain a [possibly signed] a.exe
// and a [possibly signed] b.exe. If a.exe and b.exe in
// sourceDir have the same content as each other, then
// CheckDoubleSigning checks that a.exe and b.exe in
// destinationDir have the same content as each other.
func CheckDoubleSigning(sourceDir string, destinationDir string) error {
	sourceDirHashes := NewDeepHasher()
	if err := sourceDirHashes.DeepHashDirectory(sourceDir); err != nil {
		return err
	}
	destinationDirHashes := NewDeepHasher()
	if err := destinationDirHashes.DeepHashDirectory(destinationDir); err != nil {
		return err
	}

	sourceToDestinationHashes := make(map[SHA256Hash]map[SHA256Hash]bool)
	sourceHashToPaths := make(map[SHA256Hash][]DeepPath)
	destinationHashToPaths := make(map[SHA256Hash][]DeepPath)
	for path, sourceHash := range sourceDirHashes.Hashes {
		destinationHash := destinationDirHashes.Hashes[path]
		if _, exists := sourceToDestinationHashes[sourceHash]; !exists {
			sourceToDestinationHashes[sourceHash] = make(map[SHA256Hash]bool)
		}
		sourceToDestinationHashes[sourceHash][destinationHash] = true
		sourceHashToPaths[sourceHash] = append(sourceHashToPaths[sourceHash], path)
		destinationHashToPaths[destinationHash] = append(destinationHashToPaths[destinationHash], path)
	}

	prettyPrintBadConversion := func(destinationHashes map[SHA256Hash]bool, destinationHashToPaths map[SHA256Hash][]DeepPath) {
		var previousPath *DeepPath = nil
		for destinationHash, _ := range destinationHashes {
			path := destinationHashToPaths[destinationHash][0]
			if previousPath != nil {
				log.Printf(
					"bug detected in sign-release.go: destination %v and %v have different hashes despite coming from bit-identical sources",
					path,
					*previousPath,
				)
			}
			previousPath = &path
		}
	}
	detectedBug := false
	for _, destinationHashes := range sourceToDestinationHashes {
		if len(destinationHashes) > 1 {
			detectedBug = true
			prettyPrintBadConversion(destinationHashes, destinationHashToPaths)
		}
	}
	if detectedBug {
		return fmt.Errorf("bug detected in sign-release.go")
	}
	return nil
}

// If the file is an archive and has a file which needs to be signed, sign the
// embedded file and recreate the archive. Otherwise, copy the file verbatim.
func CopyFileOrTransformArchive(deepPath DeepPath, sourcePath string, destinationPath string, sourceInfo fs.FileInfo) error {
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("expected regular file: %q", sourcePath)
	}

	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destinationFile, err := os.OpenFile(destinationPath,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	fileComplete := false
	defer (func() {
		destinationFile.Close()
		if !fileComplete {
			os.Remove(destinationPath)
		}
	})()

	transformResult, err := TransformFile(deepPath, sourceFile)
	if err != nil {
		return err
	}
	if transformResult.newFile == nil {
		_, err = io.Copy(destinationFile, sourceFile)
		if err != nil {
			return err
		}
		err = os.Chtimes(destinationPath, sourceInfo.ModTime(), sourceInfo.ModTime())
		if err != nil {
			return err
		}
		fileComplete = true
	} else {
		if _, err := destinationFile.Write(*transformResult.newFile); err != nil {
			return err
		}
		fileComplete = true
	}
	if transformResult.siblingFile != nil {
		panic("siblingFile not yet implemented for filesystem destinations")
	}

	return nil
}

type FileTransformResult struct {
	// If newFile is nil, then the file remains untouched.
	// If newFile is not nil, its contents are used.
	newFile *[]byte

	// If siblingFile is not nil, then a new file is created named
	// siblingFileName.
	siblingFile     *[]byte
	siblingFileName string
}

func (self *FileTransformResult) UpdateTarHeader(header *tar.Header) {
	if self.newFile != nil {
		if header.Format == tar.FormatGNU || header.Format == tar.FormatPAX {
			header.AccessTime = ProgramStartTime
			header.ChangeTime = ProgramStartTime
		}
		header.ModTime = ProgramStartTime
		size := len(*self.newFile)
		header.Size = int64(size)
	}
}

func (self *FileTransformResult) UpdateZipHeader(header *zip.FileHeader) {
	if self.newFile != nil {
		header.Modified = ProgramStartTime
		size := len(*self.newFile)
		header.UncompressedSize = uint32(size)
		header.UncompressedSize64 = uint64(size)
	}
}

func TransformFile(deepPath DeepPath, file io.Reader) (FileTransformResult, error) {
	var err error

	if PathLooksLikeTarGz(deepPath.Last()) {
		// TODO(strager): Optimization: Don't
		// process this file if no entry of
		// filesToTransform mentions it.
		needsTransform := true
		if needsTransform {
			transformResult, err := TransformTarGz(deepPath, file)
			if err != nil {
				return FileTransformResult{}, err
			}
			return transformResult, nil
		}
	}

	if PathLooksLikeZip(deepPath.Last()) {
		// TODO(strager): Optimization: Don't
		// process this file if no entry of
		// filesToTransform mentions it.
		needsTransform := true
		if needsTransform {
			fileContent, err := io.ReadAll(file)
			if err != nil {
				return FileTransformResult{}, err
			}

			transformResult, err := TransformZip(deepPath, fileContent)
			if err != nil {
				return FileTransformResult{}, err
			}
			return transformResult, nil
		}
	}

	transformType := filesToTransform[deepPath]

	var fileHash SHA256Hash
	if transformType != NoTransform {
		fileContent, err := io.ReadAll(file)
		if err != nil {
			return FileTransformResult{}, err
		}

		hasher := sha256.New()
		if _, err := io.Copy(hasher, bytes.NewReader(fileContent)); err != nil {
			return FileTransformResult{}, err
		}
		hashSlice := hasher.Sum(nil)
		copy(fileHash[:], hashSlice)

		cachedTransform := TransformCache[fileHash]
		if cachedTransform.newFile != nil || cachedTransform.siblingFile != nil {
			delete(filesToTransform, deepPath)
			return cachedTransform, nil
		}

		file = bytes.NewReader(fileContent)
	}

	var transform FileTransformResult
	switch transformType {
	case AppleCodesign:
		log.Printf("signing with Apple codesign: %v\n", deepPath)
		transform, err = AppleCodesignTransform(deepPath.Last(), file)
		if err != nil {
			return FileTransformResult{}, err
		}

	case GPGSign:
		log.Printf("signing with GPG: %v\n", deepPath)
		transform, err = GPGSignTransform(deepPath.Last(), file)
		if err != nil {
			return FileTransformResult{}, err
		}

	case MicrosoftOsslsigncode:
		log.Printf("signing with osslsigncode: %v\n", deepPath)
		transform, err = MicrosoftOsslsigncodeTransform(file)
		if err != nil {
			return FileTransformResult{}, err
		}

	default: // NoTransform
		return NoOpTransform(), nil
	}

	delete(filesToTransform, deepPath)
	TransformCache[fileHash] = transform
	return transform, nil
}

func TransformTarGz(
	tarGzDeepPath DeepPath,
	sourceFile io.Reader,
) (FileTransformResult, error) {
	var destinationFile bytes.Buffer
	if err := TransformTarGzToFile(tarGzDeepPath, sourceFile, &destinationFile); err != nil {
		return FileTransformResult{}, err
	}
	destinationFileContent := destinationFile.Bytes()
	return FileTransformResult{
		newFile: &destinationFileContent,
	}, nil
}

func TransformTarGzToFile(
	tarGzDeepPath DeepPath,
	sourceFile io.Reader,
	destinationFile io.Writer,
) error {
	sourceUngzippedFile, err := gzip.NewReader(sourceFile)
	if err != nil {
		return err
	}
	destinationUngzippedFile := gzip.NewWriter(destinationFile)
	defer destinationUngzippedFile.Close()
	tarReader := tar.NewReader(sourceUngzippedFile)
	tarWriter := tar.NewWriter(destinationUngzippedFile)
	defer tarWriter.Close()
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		fileContent, err := io.ReadAll(tarReader)
		if err != nil {
			return err
		}
		if int64(len(fileContent)) != header.Size {
			return fmt.Errorf("failed to read entire file")
		}

		transformResult, err := TransformFile(tarGzDeepPath.Append(header.Name), bytes.NewReader(fileContent))
		if err != nil {
			return err
		}

		transformResult.UpdateTarHeader(header)
		var newFileContent []byte = fileContent
		if transformResult.newFile != nil {
			newFileContent = *transformResult.newFile
		}
		if err := WriteTarEntry(header, newFileContent, tarWriter); err != nil {
			return err
		}

		if transformResult.siblingFile != nil {
			siblingHeader := tar.Header{
				Typeflag: tar.TypeReg,
				Name:     filepath.Join(filepath.Dir(header.Name), transformResult.siblingFileName),
				Size:     int64(len(*transformResult.siblingFile)),
				Mode:     header.Mode &^ 0111,
				Uid:      header.Uid,
				Gid:      header.Gid,
				Uname:    header.Uname,
				Gname:    header.Gname,
				ModTime:  ProgramStartTime,
				Format:   tar.FormatUSTAR,
			}
			if err := WriteTarEntry(&siblingHeader, *transformResult.siblingFile, tarWriter); err != nil {
				return err
			}
		}
	}
	return nil
}

func TransformZip(
	zipDeepPath DeepPath,
	sourceFile []byte,
) (FileTransformResult, error) {
	var destinationFile bytes.Buffer
	if err := TransformZipToFile(zipDeepPath, sourceFile, &destinationFile); err != nil {
		return FileTransformResult{}, err
	}
	destinationFileContent := destinationFile.Bytes()
	return FileTransformResult{
		newFile: &destinationFileContent,
	}, nil
}

func TransformZipToFile(
	zipDeepPath DeepPath,
	sourceFile []byte,
	destinationFile io.Writer,
) error {
	sourceZipFile, err := zip.NewReader(bytes.NewReader(sourceFile), int64(len(sourceFile)))
	if err != nil {
		return err
	}

	destinationZipFile := zip.NewWriter(destinationFile)
	defer destinationZipFile.Close()

	for _, zipEntry := range sourceZipFile.File {
		zipEntryFile, err := zipEntry.Open()
		if err != nil {
			return err
		}
		defer zipEntryFile.Close()

		transformResult, err := TransformFile(zipDeepPath.Append(zipEntry.Name), zipEntryFile)
		if err != nil {
			return err
		}

		transformResult.UpdateZipHeader(&zipEntry.FileHeader)
		if transformResult.newFile == nil {
			rawZIPEntryFile, err := zipEntry.OpenRaw()
			if err != nil {
				return err
			}

			destinationZipEntryFile, err := destinationZipFile.CreateRaw(&zipEntry.FileHeader)
			if err != nil {
				return err
			}
			_, err = io.Copy(destinationZipEntryFile, rawZIPEntryFile)
			if err != nil {
				return err
			}
		} else {
			destinationZipEntryFile, err := destinationZipFile.CreateHeader(&zipEntry.FileHeader)
			if err != nil {
				return err
			}
			if _, err := destinationZipEntryFile.Write(*transformResult.newFile); err != nil {
				return err
			}
		}

		if transformResult.siblingFile != nil {
			siblingZIPEntryFile, err := destinationZipFile.Create(filepath.Join(filepath.Dir(zipEntry.Name), transformResult.siblingFileName))
			if err != nil {
				return err
			}
			if _, err := siblingZIPEntryFile.Write(*transformResult.siblingFile); err != nil {
				return err
			}
		}
	}
	return nil
}

func NoOpTransform() FileTransformResult {
	return FileTransformResult{
		newFile: nil,
	}
}

// originalPath need not be a path to a real file.
func AppleCodesignTransform(originalPath string, exe io.Reader) (FileTransformResult, error) {
	tempDir, err := ioutil.TempDir("", "quick-lint-js-sign-release")
	if err != nil {
		return FileTransformResult{}, err
	}
	TempDirs = append(TempDirs, tempDir)

	// Name the file the same as the original. The codesign utility
	// sometimes uses the file name as the Identifier, and we don't want the
	// Identifier being some random string.
	tempFile, err := os.Create(filepath.Join(tempDir, filepath.Base(originalPath)))
	if err != nil {
		return FileTransformResult{}, err
	}
	_, err = io.Copy(tempFile, exe)
	tempFile.Close()
	if err != nil {
		return FileTransformResult{}, err
	}

	if err := AppleCodesignFile(tempFile.Name()); err != nil {
		return FileTransformResult{}, err
	}
	if err := AppleCodesignVerifyFile(tempFile.Name()); err != nil {
		return FileTransformResult{}, err
	}

	// AppleCodesignFile replaced the file, so we can't just rewind
	// tempFile. Open the file again.
	signedFileContent, err := os.ReadFile(tempFile.Name())
	if err != nil {
		return FileTransformResult{}, err
	}
	return FileTransformResult{
		newFile: &signedFileContent,
	}, nil
}

func AppleCodesignFile(filePath string) error {
	signCommand := []string{"codesign", "--sign", signingStuff.AppleCodesignIdentity, "--force", "--", filePath}
	process := exec.Command(signCommand[0], signCommand[1:]...)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}
	return nil
}

func AppleCodesignVerifyFile(filePath string) error {
	requirements := fmt.Sprintf("certificate leaf = H\"%s\"", hex.EncodeToString(signingStuff.CertificateSHA1Hash[:]))

	signCommand := []string{"codesign", "-vvv", fmt.Sprintf("-R=%s", requirements), "--", filePath}
	process := exec.Command(signCommand[0], signCommand[1:]...)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}

	return nil
}

// originalPath need not be a path to a real file.
func GPGSignTransform(originalPath string, exe io.Reader) (FileTransformResult, error) {
	tempDir, err := ioutil.TempDir("", "quick-lint-js-sign-release")
	if err != nil {
		return FileTransformResult{}, err
	}
	TempDirs = append(TempDirs, tempDir)

	tempFile, err := os.Create(filepath.Join(tempDir, "data"))
	if err != nil {
		return FileTransformResult{}, err
	}
	defer os.Remove(tempFile.Name())
	_, err = io.Copy(tempFile, exe)
	tempFile.Close()
	if err != nil {
		return FileTransformResult{}, err
	}

	signatureFilePath, err := GPGSignFile(tempFile.Name())
	if err != nil {
		return FileTransformResult{}, err
	}
	if err := GPGVerifySignature(tempFile.Name(), signatureFilePath); err != nil {
		return FileTransformResult{}, err
	}

	signatureFileContent, err := os.ReadFile(signatureFilePath)
	if err != nil {
		return FileTransformResult{}, err
	}
	return FileTransformResult{
		siblingFile:     &signatureFileContent,
		siblingFileName: filepath.Base(originalPath) + ".asc",
	}, nil
}

func GPGSignFile(filePath string) (string, error) {
	process := exec.Command(
		"gpg",
		"--local-user", signingStuff.GPGIdentity,
		"--armor",
		"--detach-sign",
		"--",
		filePath,
	)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		return "", err
	}
	if err := process.Wait(); err != nil {
		return "", err
	}
	return filePath + ".asc", nil
}

func GPGVerifySignature(filePath string, signatureFilePath string) error {
	// HACK(strager): Use /tmp instead of the default temp dir. macOS'
	// default temp dir is so long that it breaks gpg-agent.
	tempGPGHome, err := ioutil.TempDir("/tmp", "quick-lint-js-sign-release")
	if err != nil {
		return err
	}
	TempDirs = append(TempDirs, tempGPGHome)

	var env []string
	env = append([]string{}, os.Environ()...)
	env = append(env, "GNUPGHOME="+tempGPGHome)

	process := exec.Command("gpg", "--import")
	keyReader := bytes.NewReader(signingStuff.GPGKey)
	process.Stdin = keyReader
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	process.Env = env
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}

	process = exec.Command(
		"gpg", "--verify",
		"--",
		signatureFilePath,
		filePath,
	)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	process.Env = env
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}

	return nil
}

func MicrosoftOsslsigncodeTransform(exe io.Reader) (FileTransformResult, error) {
	tempDir, err := ioutil.TempDir("", "quick-lint-js-sign-release")
	if err != nil {
		return FileTransformResult{}, err
	}
	TempDirs = append(TempDirs, tempDir)

	unsignedFile, err := os.Create(filepath.Join(tempDir, "unsigned.exe"))
	if err != nil {
		return FileTransformResult{}, err
	}
	defer os.Remove(unsignedFile.Name())
	_, err = io.Copy(unsignedFile, exe)
	unsignedFile.Close()
	if err != nil {
		return FileTransformResult{}, err
	}

	signedFilePath := filepath.Join(tempDir, "signed.exe")
	if err := MicrosoftOsslsigncodeFile(unsignedFile.Name(), signedFilePath); err != nil {
		return FileTransformResult{}, err
	}
	if err := MicrosoftOsslsigncodeVerifyFile(signedFilePath); err != nil {
		return FileTransformResult{}, err
	}

	signedFileContent, err := os.ReadFile(signedFilePath)
	if err != nil {
		return FileTransformResult{}, err
	}
	return FileTransformResult{
		newFile: &signedFileContent,
	}, nil
}

func MicrosoftOsslsigncodeFile(inFilePath string, outFilePath string) error {
	signCommand := []string{
		"osslsigncode", "sign",
		"-pkcs12", signingStuff.PrivateKeyPKCS12Path,
		"-t", "http://timestamp.digicert.com",
		"-in", inFilePath,
		"-out", outFilePath,
	}
	process := exec.Command(signCommand[0], signCommand[1:]...)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}
	return nil
}

func MicrosoftOsslsigncodeVerifyFile(filePath string) error {
	certificatePEMFile, err := ioutil.TempFile("", "quick-lint-js-sign-release")
	if err != nil {
		return err
	}
	certificatePEMFile.Close()
	defer os.Remove(certificatePEMFile.Name())

	process := exec.Command(
		"openssl", "x509",
		"-inform", "der",
		"-outform", "pem",
		"-out", certificatePEMFile.Name(),
	)
	certificateReader := bytes.NewReader(signingStuff.Certificate)
	process.Stdin = certificateReader
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}

	process = exec.Command(
		"osslsigncode", "verify",
		"-in", filePath,
		"-CAfile", certificatePEMFile.Name(),
	)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}

	return nil
}

func WriteTarEntry(header *tar.Header, fileContent []byte, output *tar.Writer) error {
	if err := output.WriteHeader(header); err != nil {
		return err
	}
	bytesWritten, err := output.Write(fileContent)
	if err != nil {
		return err
	}
	if int64(bytesWritten) != header.Size {
		return fmt.Errorf("failed to write entire file")
	}
	return nil
}

type ListOfHashes struct {
	SHA256Hashes bytes.Buffer
}

func (self *ListOfHashes) AddHashOfFile(path string, name string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	self.SHA256Hashes.WriteString(fmt.Sprintf("%x", hasher.Sum(nil)))
	self.SHA256Hashes.WriteString("  ")
	self.SHA256Hashes.WriteString(name)
	self.SHA256Hashes.WriteString("\n")
	return nil
}

func (self *ListOfHashes) DumpSHA256HashesToFile(outPath string) error {
	outFile, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer outFile.Close()
	data := self.SHA256Hashes.Bytes()
	bytesWritten, err := outFile.Write(data)
	if err != nil {
		return err
	}
	if bytesWritten != len(data) {
		return fmt.Errorf("failed to write entire file")
	}
	return nil
}

func VerifySHA256SUMSFile(hashesPath string) error {
	process := exec.Command("shasum", "--algorithm", "256", "--check", "--", filepath.Base(hashesPath))
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	process.Dir = filepath.Dir(hashesPath)
	if err := process.Start(); err != nil {
		return err
	}
	if err := process.Wait(); err != nil {
		return err
	}
	return nil
}

type DeepPath struct {
	parts [3]string
}

func NewDeepPath(path string) DeepPath {
	return DeepPath{[3]string{path, "", ""}}
}

func NewDeepPath2(path0 string, path1 string) DeepPath {
	return DeepPath{[3]string{path0, path1, ""}}
}

func NewDeepPath3(path0 string, path1 string, path2 string) DeepPath {
	return DeepPath{[3]string{path0, path1, path2}}
}

func (path *DeepPath) Append(child string) DeepPath {
	newPath := *path
	if newPath.parts[0] == "" {
		newPath.parts[0] = child
	} else if path.parts[1] == "" {
		newPath.parts[1] = child
	} else if path.parts[2] == "" {
		newPath.parts[2] = child
	} else {
		log.Fatal("cannot append %#v to %#v; DeepPath has no space left", child, newPath)
	}
	return newPath
}

func (path *DeepPath) Last() string {
	if path.parts[2] != "" {
		return path.parts[2]
	}
	if path.parts[1] != "" {
		return path.parts[1]
	}
	return path.parts[0]
}

func PathLooksLikeTarGz(path string) bool {
	return strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz")
}

func PathLooksLikeZip(path string) bool {
	return strings.HasSuffix(path, ".nupkg") || strings.HasSuffix(path, ".vsix") || strings.HasSuffix(path, ".zip")
}

// quick-lint-js finds bugs in JavaScript programs.
// Copyright (C) 2020  Matthew "strager" Glazar
//
// This file is part of quick-lint-js.
//
// quick-lint-js is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// quick-lint-js is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with quick-lint-js.  If not, see <https://www.gnu.org/licenses/>.
