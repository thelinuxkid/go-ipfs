package commands

import (
	"fmt"
	"io"
	"path"

	"github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/cheggaaa/pb"

	cmds "github.com/ipfs/go-ipfs/commands"
	files "github.com/ipfs/go-ipfs/commands/files"
	core "github.com/ipfs/go-ipfs/core"
	importer "github.com/ipfs/go-ipfs/importer"
	"github.com/ipfs/go-ipfs/importer/chunk"
	dag "github.com/ipfs/go-ipfs/merkledag"
	pin "github.com/ipfs/go-ipfs/pin"
	ft "github.com/ipfs/go-ipfs/unixfs"
	u "github.com/ipfs/go-ipfs/util"
)

// Error indicating the max depth has been exceded.
var ErrDepthLimitExceeded = fmt.Errorf("depth limit exceeded")

// how many bytes of progress to wait before sending a progress update message
const progressReaderIncrement = 1024 * 256

const (
	quietOptionName    = "quiet"
	progressOptionName = "progress"
	trickleOptionName  = "trickle"
	wrapOptionName     = "wrap-with-directory"
	hiddenOptionName   = "hidden"
	onlyHashOptionName = "only-hash"
)

type AddedObject struct {
	Name  string
	Hash  string `json:",omitempty"`
	Bytes int64  `json:",omitempty"`
}

var AddCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Add an object to ipfs.",
		ShortDescription: `
Adds contents of <path> to ipfs. Use -r to add directories.
Note that directories are added recursively, to form the ipfs
MerkleDAG. A smarter partial add with a staging area (like git)
remains to be implemented.
`,
	},

	Arguments: []cmds.Argument{
		cmds.FileArg("path", true, true, "The path to a file to be added to IPFS").EnableRecursive().EnableStdin(),
	},
	Options: []cmds.Option{
		cmds.OptionRecursivePath, // a builtin option that allows recursive paths (-r, --recursive)
		cmds.BoolOption(quietOptionName, "q", "Write minimal output"),
		cmds.BoolOption(progressOptionName, "p", "Stream progress data"),
		cmds.BoolOption(trickleOptionName, "t", "Use trickle-dag format for dag generation"),
		cmds.BoolOption(onlyHashOptionName, "n", "Only chunk and hash - do not write to disk"),
		cmds.BoolOption(wrapOptionName, "w", "Wrap files with a directory object"),
		cmds.BoolOption(hiddenOptionName, "Include files that are hidden"),
	},
	PreRun: func(req cmds.Request) error {
		if quiet, _, _ := req.Option(quietOptionName).Bool(); quiet {
			return nil
		}

		req.SetOption(progressOptionName, true)

		sizeFile, ok := req.Files().(files.SizeFile)
		if !ok {
			// we don't need to error, the progress bar just won't know how big the files are
			return nil
		}

		size, err := sizeFile.Size()
		if err != nil {
			// see comment above
			return nil
		}
		log.Debugf("Total size of file being added: %v\n", size)
		req.Values()["size"] = size

		return nil
	},
	Run: func(req cmds.Request, res cmds.Response) {
		n, err := req.InvocContext().GetNode()
		if err != nil {
			res.SetError(err, cmds.ErrNormal)
			return
		}

		progress, _, _ := req.Option(progressOptionName).Bool()
		trickle, _, _ := req.Option(trickleOptionName).Bool()
		wrap, _, _ := req.Option(wrapOptionName).Bool()
		hash, _, _ := req.Option(onlyHashOptionName).Bool()
		hidden, _, _ := req.Option(hiddenOptionName).Bool()

		if hash {
			nilnode, err := core.NewNodeBuilder().NilRepo().Build(n.Context())
			if err != nil {
				res.SetError(err, cmds.ErrNormal)
				return
			}
			n = nilnode
		}

		outChan := make(chan interface{}, 8)
		res.SetOutput((<-chan interface{})(outChan))

		// addSingleFile is a function that adds a file given as a param.
		addSingleFile := func(file files.File) error {
			// lock blockstore to prevent rogue GC
			unlock := n.DataBlocks.PinLock()
			defer unlock()
			sunlock := n.StateBlocks.PinLock()
			defer sunlock()

			addParams := adder{
				node:     n,
				out:      outChan,
				progress: progress,
				hidden:   hidden,
				trickle:  trickle,
			}

			rootnd, err := addParams.addFile(file)
			if err != nil {
				return err
			}

			rnk, err := rootnd.Key()
			if err != nil {
				return err
			}

			if !hash {
				n.Pinning.PinWithMode(rnk, pin.Recursive)
				err = n.Pinning.Flush()
				if err != nil {
					return err
				}
			}

			return nil
		}

		// addFilesSeparately loops over a convenience slice file to
		// add each file individually. e.g. 'ipfs add a b c'
		addFilesSeparately := func(sliceFile files.File) error {
			for {
				file, err := sliceFile.NextFile()
				if err != nil && err != io.EOF {
					return err
				}
				if file == nil {
					return nil // done
				}

				if err := addSingleFile(file); err != nil {
					return err
				}
			}
		}

		go func() {
			defer close(outChan)

			// really, we're unrapping, if !wrap, because
			// req.Files() is already a SliceFile() with all of them,
			// so can just use that slice as the wrapper.
			var err error
			if wrap {
				err = addSingleFile(req.Files())
			} else {
				err = addFilesSeparately(req.Files())
			}
			if err != nil {
				res.SetError(err, cmds.ErrNormal)
				return
			}
		}()
	},
	PostRun: func(req cmds.Request, res cmds.Response) {
		if res.Error() != nil {
			return
		}
		outChan, ok := res.Output().(<-chan interface{})
		if !ok {
			res.SetError(u.ErrCast(), cmds.ErrNormal)
			return
		}
		res.SetOutput(nil)

		quiet, _, err := req.Option("quiet").Bool()
		if err != nil {
			res.SetError(u.ErrCast(), cmds.ErrNormal)
			return
		}

		size := int64(0)
		s, found := req.Values()["size"]
		if found {
			size = s.(int64)
		}
		showProgressBar := !quiet && size >= progressBarMinSize

		var bar *pb.ProgressBar
		var terminalWidth int
		if showProgressBar {
			bar = pb.New64(size).SetUnits(pb.U_BYTES)
			bar.ManualUpdate = true
			bar.Start()

			// the progress bar lib doesn't give us a way to get the width of the output,
			// so as a hack we just use a callback to measure the output, then git rid of it
			terminalWidth = 0
			bar.Callback = func(line string) {
				terminalWidth = len(line)
				bar.Callback = nil
				bar.Output = res.Stderr()
				log.Infof("terminal width: %v\n", terminalWidth)
			}
			bar.Update()
		}

		lastFile := ""
		var totalProgress, prevFiles, lastBytes int64

		for out := range outChan {
			output := out.(*AddedObject)
			if len(output.Hash) > 0 {
				if showProgressBar {
					// clear progress bar line before we print "added x" output
					fmt.Fprintf(res.Stderr(), "\033[2K\r")
				}
				if quiet {
					fmt.Fprintf(res.Stdout(), "%s\n", output.Hash)
				} else {
					fmt.Fprintf(res.Stdout(), "added %s %s\n", output.Hash, output.Name)
				}

			} else {
				log.Debugf("add progress: %v %v\n", output.Name, output.Bytes)

				if !showProgressBar {
					continue
				}

				if len(lastFile) == 0 {
					lastFile = output.Name
				}
				if output.Name != lastFile || output.Bytes < lastBytes {
					prevFiles += lastBytes
					lastFile = output.Name
				}
				lastBytes = output.Bytes
				delta := prevFiles + lastBytes - totalProgress
				totalProgress = bar.Add64(delta)
			}

			if showProgressBar {
				bar.Update()
			}
		}
	},
	Type: AddedObject{},
}

// Internal structure for holding the switches passed to the `add` call
type adder struct {
	node     *core.IpfsNode
	out      chan interface{}
	progress bool
	hidden   bool
	trickle  bool
}

// Perform the actual add & pin locally, outputting results to reader
func add(n *core.IpfsNode, reader io.Reader, useTrickle bool) (*dag.Node, error) {
	var node *dag.Node
	var err error
	if useTrickle {
		node, err = importer.BuildTrickleDagFromReader(
			reader,
			n.DAG,
			chunk.DefaultSplitter,
		)
	} else {
		node, err = importer.BuildDagFromReader(
			reader,
			n.DAG,
			chunk.DefaultSplitter,
		)
	}

	if err != nil {
		return nil, err
	}

	return node, nil
}

// Add the given file while respecting the params.
func (params *adder) addFile(file files.File) (*dag.Node, error) {
	// Check if file is hidden
	if fileIsHidden := files.IsHidden(file); fileIsHidden && !params.hidden {
		log.Debugf("%s is hidden, skipping", file.FileName())
		return nil, &hiddenFileError{file.FileName()}
	}

	// Check if "file" is actually a directory
	if file.IsDirectory() {
		return params.addDir(file)
	}

	// if the progress flag was specified, wrap the file so that we can send
	// progress updates to the client (over the output channel)
	var reader io.Reader = file
	if params.progress {
		reader = &progressReader{file: file, out: params.out}
	}

	dagnode, err := add(params.node, reader, params.trickle)
	if err != nil {
		return nil, err
	}

	log.Infof("adding file: %s", file.FileName())
	if err := outputDagnode(params.out, file.FileName(), dagnode); err != nil {
		return nil, err
	}
	return dagnode, nil
}

func (params *adder) addDir(file files.File) (*dag.Node, error) {
	tree := &dag.Node{Data: ft.FolderPBData()}
	log.Infof("adding directory: %s", file.FileName())

	for {
		file, err := file.NextFile()
		if err != nil && err != io.EOF {
			return nil, err
		}
		if file == nil {
			break
		}

		node, err := params.addFile(file)
		if _, ok := err.(*hiddenFileError); ok {
			// hidden file error, set the node to nil for below
			node = nil
		} else if err != nil {
			return nil, err
		}

		if node != nil {
			_, name := path.Split(file.FileName())

			err = tree.AddNodeLink(name, node)
			if err != nil {
				return nil, err
			}
		}
	}

	err := outputDagnode(params.out, file.FileName(), tree)
	if err != nil {
		return nil, err
	}

	_, err = params.node.DAG.Add(tree)
	if err != nil {
		return nil, err
	}

	return tree, nil
}

// outputDagnode sends dagnode info over the output channel
func outputDagnode(out chan interface{}, name string, dn *dag.Node) error {
	o, err := getOutput(dn)
	if err != nil {
		return err
	}

	out <- &AddedObject{
		Hash: o.Hash,
		Name: name,
	}

	return nil
}

type hiddenFileError struct {
	fileName string
}

func (e *hiddenFileError) Error() string {
	return fmt.Sprintf("%s is a hidden file", e.fileName)
}

type ignoreFileError struct {
	fileName string
}

func (e *ignoreFileError) Error() string {
	return fmt.Sprintf("%s is an ignored file", e.fileName)
}

type progressReader struct {
	file         files.File
	out          chan interface{}
	bytes        int64
	lastProgress int64
}

func (i *progressReader) Read(p []byte) (int, error) {
	n, err := i.file.Read(p)

	i.bytes += int64(n)
	if i.bytes-i.lastProgress >= progressReaderIncrement || err == io.EOF {
		i.lastProgress = i.bytes
		i.out <- &AddedObject{
			Name:  i.file.FileName(),
			Bytes: i.bytes,
		}
	}

	return n, err
}
