/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/spf13/cobra"

	webp "github.com/gen2brain/webp"
	"github.com/nfnt/resize"
	xwebp "golang.org/x/image/webp"
)

// defaultOriginalQuality is the encode quality applied to the full-size
// "original" variant for lossy formats (JPEG, WebP) when neither the --quality
// flag nor gothic.config.go's OptimizeImages.Quality is set. ~80 is a strong
// visual/size sweet spot and stops a detailed image from being emitted
// near-lossless (which balloons a 90 KB WebP source to ~1.7 MB).
const defaultOriginalQuality = 80

// blurredQuality is the (deliberately low) quality for the tiny blurred
// placeholder. It is already downscaled to LowResolutionRate%, so a low quality
// keeps it a few KB with no visible cost behind the blur.
const blurredQuality = 20

// optimizeImagesQuality is bound to the --quality/-q flag. 0 means "unset" — fall
// back to gothic.config.go's OptimizeImages.Quality, then to defaultOriginalQuality.
var optimizeImagesQuality int

var optimizeImagesCmd = &cobra.Command{
	Use:   "optimize-images",
	Short: "Generate optimized and blurred image variants in the public folder",
	Long: `Scans the "optimize" folder and generates optimized image assets in the "public" directory.
For each image, a corresponding folder is created containing:

  - The original image, re-encoded at a configurable quality (lossy formats: JPEG, WebP)
  - A blurred version (default: 20% of the original resolution)

Configure the blur resolution with OptimizeImages.LowResolutionRate and the original
encode quality with OptimizeImages.Quality in gothic.config.go (or the --quality flag).`,
	RunE: newOptimizeImagesCommand(gothic_cli.NewCli()),
}

func init() {
	optimizeImagesCmd.Flags().IntVarP(&optimizeImagesQuality, "quality", "q", 0,
		"encode quality (1-100) for the full-size original of lossy formats (JPEG, WebP); overrides gothic.config.go; default 80")
	rootCmd.AddCommand(optimizeImagesCmd)
}

type ImgOptimizationCommand struct {
	cli       *gothic_cli.GothicCli
	inputDir  string
	outputDir string
	// quality is the resolved original-encode quality for lossy formats, clamped
	// to [1,100]. Set in OptimizeImages after resolving flag/config/default.
	quality int
}

func NewImgOptimizationCommandCli(cli *gothic_cli.GothicCli) ImgOptimizationCommand {
	return ImgOptimizationCommand{
		cli:       cli,
		inputDir:  "./optimize",
		outputDir: "./public",
	}
}

func newOptimizeImagesCommand(cli gothic_cli.GothicCli) RunEFunc {
	return func(cmd *cobra.Command, args []string) error {
		command := NewImgOptimizationCommandCli(&cli)

		return command.OptimizeImages()
	}
}

// clampQuality bounds q into the valid [1,100] range.
func clampQuality(q int) int {
	if q < 1 {
		return 1
	}
	if q > 100 {
		return 100
	}
	return q
}

// resolveQuality picks the original-encode quality: the --quality flag wins, then
// gothic.config.go's OptimizeImages.Quality, then defaultOriginalQuality. The
// result is clamped to [1,100].
func resolveQuality(configQuality int) int {
	switch {
	case optimizeImagesQuality > 0:
		return clampQuality(optimizeImagesQuality)
	case configQuality > 0:
		return clampQuality(configQuality)
	default:
		return defaultOriginalQuality
	}
}

func (command *ImgOptimizationCommand) OptimizeImages() error {

	config, err := command.cli.GetConfig()
	if err != nil {
		return err
	}

	command.quality = resolveQuality(config.OptimizeImages.Quality)

	// Create the output directory if it doesn't exist
	if err := os.MkdirAll(command.outputDir, os.ModePerm); err != nil {
		return err
	}

	// Read the files from the input directory
	files, err := os.ReadDir(command.inputDir)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			return fmt.Errorf("error the 'optimizeImages' key was not found in gothic-config.json")
		}
		if err := command.optimizeOne(config, file.Name()); err != nil {
			return err
		}
	}

	fmt.Println("Resizing complete!")
	return nil
}

// optimizeOne processes a single source image into public/<basename>/{original,blurred}.<ext>.
func (command *ImgOptimizationCommand) optimizeOne(config gothic_cli.Config, fileName string) error {
	inputPath := filepath.Join(command.inputDir, fileName)
	baseName := fileName[:len(fileName)-len(filepath.Ext(fileName))]
	imageDir := filepath.Join(command.outputDir, baseName)
	ext := filepath.Ext(fileName)

	if err := os.MkdirAll(imageDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating directory %s: %v", imageDir, err)
	}

	// Open + decode the source image.
	imgFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("error opening file %s: %v", inputPath, err)
	}
	var img image.Image
	switch ext {
	case ".png":
		img, err = png.Decode(imgFile)
	case ".jpg", ".jpeg":
		img, err = jpeg.Decode(imgFile)
	case ".webp":
		img, err = xwebp.Decode(imgFile)
	default:
		imgFile.Close()
		return fmt.Errorf("error unsupported file format: %s", ext)
	}
	imgFile.Close()
	if err != nil {
		return fmt.Errorf("error decoding image %s: %v", inputPath, err)
	}

	// Blurred placeholder: LowResolutionRate% of the original dimensions.
	lowResolutionRate := 20 // default value
	if config.OptimizeImages.LowResolutionRate > 0 {
		lowResolutionRate = config.OptimizeImages.LowResolutionRate
	}
	newWidth := uint(img.Bounds().Dx()) * uint(lowResolutionRate) / 100
	newHeight := uint(img.Bounds().Dy()) * uint(lowResolutionRate) / 100
	resizedImg := resize.Resize(newWidth, newHeight, img, resize.Lanczos3)

	originalPath := filepath.Join(imageDir, "original"+ext)
	blurredPath := filepath.Join(imageDir, "blurred"+ext)

	switch ext {
	case ".png":
		// PNG is lossless — the quality knob does not apply. Emit as-is.
		if err := encodeToFile(originalPath, func(f *os.File) error { return png.Encode(f, img) }); err != nil {
			return fmt.Errorf("error saving original PNG image %s: %v", originalPath, err)
		}
		if err := encodeToFile(blurredPath, func(f *os.File) error { return png.Encode(f, resizedImg) }); err != nil {
			return fmt.Errorf("error saving blurred PNG image %s: %v", blurredPath, err)
		}
	case ".jpg", ".jpeg":
		if err := encodeToFile(originalPath, func(f *os.File) error {
			return jpeg.Encode(f, img, &jpeg.Options{Quality: command.quality})
		}); err != nil {
			return fmt.Errorf("error saving original JPEG image %s: %v", originalPath, err)
		}
		if err := encodeToFile(blurredPath, func(f *os.File) error {
			return jpeg.Encode(f, resizedImg, &jpeg.Options{Quality: blurredQuality})
		}); err != nil {
			return fmt.Errorf("error saving blurred JPEG image %s: %v", blurredPath, err)
		}
	case ".webp":
		// Real lossy WebP at the configured quality. (Previously this re-encoded
		// the original as LOSSLESS PNG bytes written into a .webp file, which
		// ballooned a detailed image to several MB — the root cause this fixes.)
		if err := encodeToFile(originalPath, func(f *os.File) error {
			return webp.Encode(f, img, webp.Options{Quality: command.quality})
		}); err != nil {
			return fmt.Errorf("error saving original WebP image %s: %v", originalPath, err)
		}
		if err := encodeToFile(blurredPath, func(f *os.File) error {
			return webp.Encode(f, resizedImg, webp.Options{Quality: blurredQuality})
		}); err != nil {
			return fmt.Errorf("error saving blurred WebP image %s: %v", blurredPath, err)
		}
	}
	return nil
}

// encodeToFile creates path, runs enc against the open file, and closes it —
// avoiding the deferred-close-in-a-loop pattern (which would keep every file
// handle open until the whole command returns).
func encodeToFile(path string, enc func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("error creating file %s: %v", path, err)
	}
	if err := enc(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
