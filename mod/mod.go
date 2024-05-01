// Package mod changes an image according to the requested modifications.
package mod

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/pkg/archive"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/mediatype"
	"github.com/regclient/regclient/types/ref"
)

// Opts defines options for Apply
type Opts func(*dagConfig, *dagManifest) error

// OptTime defines time settings for [WithConfigTimestamp] and [WithLayerTimestamp].
type OptTime struct {
	Set        time.Time // time to set, this or FromLabel are required
	FromLabel  string    // label from which to extract set time
	After      time.Time // only change times that are after this
	BaseRef    ref.Ref   // define base image, do not alter timestamps from base layers
	BaseLayers int       // define a number of layers to not modify (count of the layers in a base image)
}

var (
	// known tar media types
	mtKnownTar = []string{
		mediatype.Docker2Layer,
		mediatype.Docker2LayerGzip,
		mediatype.OCI1Layer,
		mediatype.OCI1LayerGzip,
	}
	// known config media types
	mtKnownConfig = []string{
		mediatype.Docker2ImageConfig,
		mediatype.OCI1ImageConfig,
	}
)

// Apply applies a set of modifications to an image (manifest, configs, and layers).
func Apply(ctx context.Context, rc *regclient.RegClient, rSrc ref.Ref, opts ...Opts) (ref.Ref, error) {
	// check for the various types of mods (manifest, config, layer)
	// some may span like copying layers from config to manifest
	// run changes in order (deleting layers before pulling and changing a layer)
	// to span steps, some changes will output other mods to apply
	// e.g. layer hash changing in config, or a deleted layer from the config deleting from the manifest
	// do I need to store a DAG in memory with pointers back to parents and modified bool, so change to digest can be rippled up and modified objects are pushed?

	// pull the image metadata into a DAG
	dm, err := dagGet(ctx, rc, rSrc, descriptor.Descriptor{})
	if err != nil {
		return rSrc, err
	}
	dm.top = true

	// load the options
	rTgt := rSrc
	rTgt.Tag = ""
	rTgt.Digest = ""
	dc := dagConfig{
		stepsManifest:  []func(context.Context, *regclient.RegClient, ref.Ref, ref.Ref, *dagManifest) error{},
		stepsOCIConfig: []func(context.Context, *regclient.RegClient, ref.Ref, ref.Ref, *dagOCIConfig) error{},
		stepsLayer:     []func(context.Context, *regclient.RegClient, ref.Ref, ref.Ref, *dagLayer, io.ReadCloser) (io.ReadCloser, error){},
		stepsLayerFile: []func(context.Context, *regclient.RegClient, ref.Ref, ref.Ref, *dagLayer, *tar.Header, io.Reader) (*tar.Header, io.Reader, changes, error){},
		maxDataSize:    -1, // unchanged, if a data field exists, preserve it
		rTgt:           rTgt,
	}
	for _, opt := range opts {
		if err := opt(&dc, dm); err != nil {
			return rSrc, err
		}
	}
	rTgt = dc.rTgt

	// perform manifest changes
	if len(dc.stepsManifest) > 0 {
		err = dagWalkManifests(dm, func(dm *dagManifest) (*dagManifest, error) {
			for _, fn := range dc.stepsManifest {
				err := fn(ctx, rc, rSrc, rTgt, dm)
				if err != nil {
					return nil, err
				}
			}
			return dm, nil
		})
		if err != nil {
			return rTgt, err
		}
	}
	if len(dc.stepsOCIConfig) > 0 {
		err = dagWalkOCIConfig(dm, func(doc *dagOCIConfig) (*dagOCIConfig, error) {
			for _, fn := range dc.stepsOCIConfig {
				err := fn(ctx, rc, rSrc, rTgt, doc)
				if err != nil {
					return nil, err
				}
			}
			return doc, nil
		})
		if err != nil {
			return rTgt, err
		}
	}
	if len(dc.stepsLayer) > 0 || len(dc.stepsLayerFile) > 0 || !ref.EqualRepository(rSrc, rTgt) || dc.forceLayerWalk {
		err = dagWalkLayers(dm, func(dl *dagLayer) (*dagLayer, error) {
			var rdr io.ReadCloser
			defer func() {
				if rdr != nil {
					_ = rdr.Close()
				}
			}()
			var err error
			rSrc := rSrc
			if dl.rSrc.IsSet() {
				rSrc = dl.rSrc
			}
			if dl.mod == deleted || len(dl.desc.URLs) > 0 {
				// skip deleted and external layers
				return dl, nil
			}
			if len(dc.stepsLayer) > 0 {
				rdr, err = rc.BlobGet(ctx, rSrc, dl.desc)
				if err != nil {
					return nil, err
				}
				for _, sl := range dc.stepsLayer {
					rdrNext, err := sl(ctx, rc, rSrc, rTgt, dl, rdr)
					if err != nil {
						return nil, err
					}
					rdr = rdrNext
				}
			}
			if len(dc.stepsLayerFile) > 0 && inListStr(dl.desc.MediaType, mtKnownTar) {
				if dl.mod == deleted {
					return dl, nil
				}
				if rdr == nil {
					rdr, err = rc.BlobGet(ctx, rSrc, dl.desc)
					if err != nil {
						return nil, err
					}
				}
				changed := false
				empty := true
				mt := dl.desc.MediaType
				if dl.newDesc.MediaType != "" {
					mt = dl.newDesc.MediaType
				}
				// if compressed, setup a decompressing reader that passes through the close
				if mt != mediatype.OCI1Layer && mt != mediatype.Docker2Layer {
					dr, err := archive.Decompress(rdr)
					if err != nil {
						return nil, err
					}
					rdr = readCloserFn{Reader: dr, closeFn: rdr.Close}
				}
				// setup tar reader to process layer
				tr := tar.NewReader(rdr)
				// create temp file and setup tar writer
				fh, err := os.CreateTemp("", "regclient-mod-")
				if err != nil {
					return nil, err
				}
				defer func() {
					_ = fh.Close()
					_ = os.Remove(fh.Name())
				}()
				var tw *tar.Writer
				var gw *gzip.Writer
				digRaw := digest.Canonical.Digester() // raw/compressed digest
				digUC := digest.Canonical.Digester()  // uncompressed digest
				if dl.desc.MediaType == mediatype.Docker2LayerGzip || dl.desc.MediaType == mediatype.OCI1LayerGzip {
					cw := io.MultiWriter(fh, digRaw.Hash())
					gw = gzip.NewWriter(cw)
					defer gw.Close()
					ucw := io.MultiWriter(gw, digUC.Hash())
					tw = tar.NewWriter(ucw)
				} else {
					dw := io.MultiWriter(fh, digRaw.Hash(), digUC.Hash())
					tw = tar.NewWriter(dw)
				}
				// iterate over files in the layer
				for {
					th, err := tr.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						return nil, err
					}
					changeFile := unchanged
					var rdr io.Reader
					rdr = tr
					for _, slf := range dc.stepsLayerFile {
						var changeCur changes
						th, rdr, changeCur, err = slf(ctx, rc, rSrc, rTgt, dl, th, rdr)
						if err != nil {
							return nil, err
						}
						if changeCur != unchanged {
							changed = true
						}
						if changeCur == deleted {
							changeFile = deleted
							break
						}
					}
					// copy th and tr to temp tar writer file
					if changeFile != deleted {
						empty = false
						err = tw.WriteHeader(th)
						if err != nil {
							return nil, err
						}
						if th.Typeflag == tar.TypeReg && th.Size > 0 {
							_, err := io.CopyN(tw, rdr, th.Size)
							if err != nil {
								return nil, err
							}
						}
					}
				}
				if empty {
					dl.mod = deleted
					return dl, nil
				}
				if changed {
					// close to flush remaining content
					err = tw.Close()
					if err != nil {
						return nil, fmt.Errorf("failed to close temporary tar layer: %w", err)
					}
					if gw != nil {
						err = gw.Close()
						if err != nil {
							return nil, fmt.Errorf("failed to close gzip writer: %w", err)
						}
					}
					err = rdr.Close()
					if err != nil {
						return nil, fmt.Errorf("failed to close layer reader: %w", err)
					}
					// replace the current reader and save the digests
					l, err := fh.Seek(0, 1)
					if err != nil {
						return nil, err
					}
					_, err = fh.Seek(0, 0)
					if err != nil {
						return nil, err
					}
					rdr = fh
					dl.newDesc = descriptor.Descriptor{
						MediaType: mt,
						Digest:    digRaw.Digest(),
						Size:      l,
					}
					dl.ucDigest = digUC.Digest()
					if dl.mod == unchanged {
						dl.mod = replaced
					}
				}
			}
			// if added or replaced, and reader not nil, push blob
			if (dl.mod == added || dl.mod == replaced) && rdr != nil {
				// push the blob and verify the results
				dNew, err := rc.BlobPut(ctx, rTgt, descriptor.Descriptor{}, rdr)
				if err != nil {
					return nil, err
				}
				err = rdr.Close()
				if err != nil {
					return nil, err
				}
				if dl.newDesc.Digest == "" {
					dl.newDesc.Digest = dNew.Digest
				} else if dl.newDesc.Digest != dNew.Digest {
					return nil, fmt.Errorf("layer digest mismatch, pushed %s, expected %s", dNew.Digest.String(), dl.newDesc.Digest.String())
				}
				if dl.newDesc.Size == 0 {
					dl.newDesc.Size = dNew.Size
				} else if dl.newDesc.Size != dNew.Size {
					return nil, fmt.Errorf("layer size mismatch, pushed %d, expected %d", dNew.Size, dl.newDesc.Size)
				}
			}
			if dl.mod == unchanged && !ref.EqualRepository(rSrc, rTgt) {
				err = rc.BlobCopy(ctx, rSrc, rTgt, dl.desc)
				if err != nil {
					return nil, err
				}
			}
			return dl, nil
		})
		if err != nil {
			return rTgt, err
		}
	}

	err = dagPut(ctx, rc, dc, rSrc, rTgt, dm)
	if err != nil {
		return rTgt, err
	}
	if rTgt.Tag == "" {
		rTgt.Digest = dm.m.GetDescriptor().Digest.String()
	}
	return rTgt, nil
}

// WithRefTgt sets the target manifest.
// Apply will default to pushing to the same name by digest.
func WithRefTgt(rTgt ref.Ref) Opts {
	return func(dc *dagConfig, dm *dagManifest) error {
		dc.rTgt = rTgt
		return nil
	}
}

// WithData sets the descriptor data field max size.
// This also strips the data field off descriptors above the max size.
func WithData(maxDataSize int64) Opts {
	return func(dc *dagConfig, dm *dagManifest) error {
		dc.maxDataSize = maxDataSize
		return nil
	}
}

func inListStr(str string, list []string) bool {
	for _, s := range list {
		if str == s {
			return true
		}
	}
	return false
}

func eqStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
