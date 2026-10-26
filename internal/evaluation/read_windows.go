package evaluation

import "os"

func openArtifact(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
