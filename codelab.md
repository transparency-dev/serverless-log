# Codelab

Throughout this codelab, you'll create a [Tiled tree](https://research.swtch.com/tlog#tiling_a_log).

The Tiled tree will be stored on disk using the layout described in the [layout
directory](api/layout/README.md). Its checkpoint uses the [checkpoint format](https://github.com/transparency-dev/formats/blob/main/log/README.md#checkpoint-format).

## Prelimiary setup

The command-line tools we'll use from this repository can generate tile based logs from leaf
data stored on your file system. Each file will correspond to a single leaf in
the tree.

Before we start, let's define a few environment variables:

```bash
export DATA_DIR="/tmp/myfiles"  # where we'll store input data for the tree
export LOG_DIR="/tmp/mylog"  # where the tree will be stored
export LOG_ORIGIN="My Log"  # the origin of the log used by the Checkpoint format
```

Checkpoints of the log will be signed, and we need a public/private key pair for this.

Use the `generate_keys` command with `--key_name`, a name
for the signing entity. You can output the public and private keys to files using
`--out_pub` path and filename for the public key,
`--out_priv` path and filename for the private key
and stdout, private key, then public key, over 2 lines, using `--print`

```bash
go run ./cmd/generate_keys --key_name=astra --out_pub=key.pub --out_priv=key
```

### Creating a new log

To create a new log state directory, use the `integrate` command with the `--initialise`
flag, and either passing key files or with environment variables set:

```bash
go run ./cmd/integrate --initialise --storage_dir="${LOG_DIR}" --public_key=key.pub --private_key=key --origin="${LOG_ORIGIN}"
```

After running this command, the log state directory looks like this:

```
$ tree /tmp/mylog/
/tmp/mylog/
├── checkpoint
├── leaves
│   └── pending
├── seq
└── tile

5 directories, 1 file
```
  - `checkpoint` contains the latest log checkpoint in the format described [here](https://github.com/transparency-dev/formats/tree/main/log).
  - `seq/` contains a directory hierarchy containing leaf data for each sequenced entry in the log.
  - `leaves/` contains files which map all known leaf hashes to their position in the log.
  - `tile/` contains the internal nodes of the log tree.

See the [layout](api/layout/README.md) documentation for more details about each directory.

Let's look at the checkpoint content:

```bash
$ cat /tmp/mylog/checkpoint
My Log
0
47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=

— astra PlUh/n54e2dSIKi6kHjea5emrGnmC7lJVDgnIfWGIJmgFqp22k0UlnUk97L2ViqrFm986NwV+wJYGnrtRPJTBV0GrA0=
```

- `My Log` is the origin that we defined above
- `0` is the number of leaves in the tree, which currently is 0
- `47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=` is the [hash of an empty slice of bytes](https://go.dev/play/p/imi_2TM6DyI), since the log is empty.
- The last line is a signature over this data, using the astra private key we've generated above


### Creating log content
Now let's add some leaves to the log.

First, we generate the input data with:
```bash
$ mkdir $DATA_DIR
$ for i in $(seq 0 3); do x=$(printf "%03d" $i); echo "leaf_data_$x" > $DATA_DIR/leaf_$x; done;
```

To add the contents of these files to the log, use the `sequence` command with the
`--entries` flag set to a filename glob of files to add:

```bash
$ go run ./cmd/sequence --storage_dir="${LOG_DIR}" --entries "${DATA_DIR}/*"  --public_key=key.pub --origin="${LOG_ORIGIN}"
I1221 13:16:23.940255  923589 main.go:131] 0: /tmp/myfiles/leaf_000
I1221 13:16:23.940806  923589 main.go:131] 1: /tmp/myfiles/leaf_001
I1221 13:16:23.941218  923589 main.go:131] 2: /tmp/myfiles/leaf_002
I1221 13:16:23.941673  923589 main.go:131] 3: /tmp/myfiles/leaf_003
```

The `sequence` commands assigns an index to each leaf, and stores data in the log directory using convenient
formats.

Here is what the directory looks like:

```bash
$ grep -RH '^' /tmp/mylog/
/tmp/mylog/checkpoint:My Log
/tmp/mylog/checkpoint:0
/tmp/mylog/checkpoint:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=
/tmp/mylog/checkpoint:
/tmp/mylog/checkpoint:— astra h5lA3N6MJnmnD1dPLqxeoWbbPAc0XPKuqomvSZPiVNLkdJmPDvF+7BkMIr4KBynVgo/ipGbNijHxdbvTZ4zKVXbyLwU=
/tmp/mylog/leaves/6c/b0/b1/a3c33114cec1d940b9a6c48b55fb2c73f6efcfd53aeef2644681c9b70a:2
/tmp/mylog/leaves/b8/71/4f/045c7d5d0201b06004e6939d944a981605c5fcfa5d3353a3084303d4ad:1
/tmp/mylog/leaves/85/92/d6/f366d9d1297f44034d649b68afcee74050aa7a55c769130b2f07ecc65d:0
/tmp/mylog/leaves/e0/7c/75/881e1ec1bcad5e45c5cc3d8e2c83cda817a48324514309267ee32ef115:3
/tmp/mylog/seq/00/00/00/00/02:leaf_data_002
/tmp/mylog/seq/00/00/00/00/00:leaf_data_000
/tmp/mylog/seq/00/00/00/00/01:leaf_data_001
/tmp/mylog/seq/00/00/00/00/03:leaf_data_003
```

The `seq` directory contains the leaves data, in files named after each leaf's index.

The `leaves` stores the leaf index of each leaf, in a file named after the leaf hash.
Let's take the leaf at index `0`, which conveniently happens to contain `leaf_data_000`.
This tree uses [RFC6962's hashing function](https://www.rfc-editor.org/rfc/rfc6962#page-4), where `leaf_hash = sha256(0x + leaf_data)`.

`8592d6f366d9d1297f44034d649b68afcee74050aa7a55c769130b2f07ecc65d`, the path for
the leaf at index 0 with forward slashes removed, is the [hexadecimal representation
of this hash](https://go.dev/play/p/POnCQ7IXayk).

Note that at this point, no internal node of the tree has been computed, and neither
has the checkpoint been updated. Leaves have only been assigned with a position
in the log.

Attempting to re-sequence the same file contents will result in the `sequence`
tool telling you that you're trying to add duplicate entries, along with their
originally assigned sequence numbers:

```bash
$ go run ./cmd/sequence --storage_dir="${LOG_DIR}" --entries "${DATA_DIR}/*"  --public_key=key.pub --origin="${LOG_ORIGIN}"
I1221 13:18:59.735244  924268 main.go:131] 0: /tmp/myfiles/leaf_000 (dupe)
I1221 13:18:59.735362  924268 main.go:131] 1: /tmp/myfiles/leaf_001 (dupe)
I1221 13:18:59.735406  924268 main.go:131] 2: /tmp/myfiles/leaf_002 (dupe)
I1221 13:18:59.735447  924268 main.go:131] 3: /tmp/myfiles/leaf_003 (dupe)
```

### Integrating sequenced entries

We still need to update the rest of the tree structure to integrate these new entries, generate the other nodes of the tree, and compute its new checkpoint.
We use the `integrate` tool for that:

```bash
$ go run ./cmd/integrate  --storage_dir="${LOG_DIR}"  --public_key=key.pub --private_key=key --origin="${LOG_ORIGIN}"
I1221 13:19:20.190193  924589 integrate.go:94] Loaded state with roothash
I1221 13:19:20.190432  924589 integrate.go:132] New log state: size 0x4 hash: 0c2e71ac054d92d58b0efd3013d0df235245331f0c0e828bab62a8fe62460c7f
```

This output says that the integration was successful, and we now have a new log
tree state which contains 4 entries, and has the printed log root hash.

Let's look at the contents of the tree directory again:

```bash
$ grep -RH '^' /tmp/mylog/
/tmp/mylog/tile/00/0000/00/00/00.04:32
/tmp/mylog/tile/00/0000/00/00/00.04:4
/tmp/mylog/tile/00/0000/00/00/00.04:hZLW82bZ0Sl/RANNZJtor87nQFCqelXHaRMLLwfsxl0=
/tmp/mylog/tile/00/0000/00/00/00.04:McF1R3nScwEJFHQpESACDl9SOdg9uTRLVZaDHzLckI0=
/tmp/mylog/tile/00/0000/00/00/00.04:uHFPBFx9XQIBsGAE5pOdlEqYFgXF/PpdM1OjCEMD1K0=
/tmp/mylog/tile/00/0000/00/00/00.04:DC5xrAVNktWLDv0wE9DfI1JFMx8MDoKLq2Ko/mJGDH8=
/tmp/mylog/tile/00/0000/00/00/00.04:bLCxo8MxFM7B2UC5psSLVfssc/bvz9U67vJkRoHJtwo=
/tmp/mylog/tile/00/0000/00/00/00.04:jNfnGF6uHUDupKFIaPW/QjZnPkINVKkVYc7cBakvPy4=
/tmp/mylog/tile/00/0000/00/00/00.04:4Hx1iB4ewbytXkXFzD2OLIPNqBekgyRRQwkmfuMu8RU=
/tmp/mylog/checkpoint:My Log
/tmp/mylog/checkpoint:4
/tmp/mylog/checkpoint:DC5xrAVNktWLDv0wE9DfI1JFMx8MDoKLq2Ko/mJGDH8=
/tmp/mylog/checkpoint:
/tmp/mylog/checkpoint:— astra h5lA3GOB547TCfoNMEXxENGJVWmpG6Ynk8C6Oaef5gaFotSVLX9isWdvjnhBek94Is9yVPzIvjQTADF/dk2MhHXiCAY=
/tmp/mylog/leaves/6c/b0/b1/a3c33114cec1d940b9a6c48b55fb2c73f6efcfd53aeef2644681c9b70a:2
/tmp/mylog/leaves/b8/71/4f/045c7d5d0201b06004e6939d944a981605c5fcfa5d3353a3084303d4ad:1
/tmp/mylog/leaves/85/92/d6/f366d9d1297f44034d649b68afcee74050aa7a55c769130b2f07ecc65d:0
/tmp/mylog/leaves/e0/7c/75/881e1ec1bcad5e45c5cc3d8e2c83cda817a48324514309267ee32ef115:3
/tmp/mylog/seq/00/00/00/00/02:leaf_data_002
/tmp/mylog/seq/00/00/00/00/00:leaf_data_000
/tmp/mylog/seq/00/00/00/00/01:leaf_data_001
/tmp/mylog/seq/00/00/00/00/03:leaf_data_003
```

The tile directory has been populated with a file, and the checkpoint has been updated.
The `leaves/` and `seq/` directories have not changed.

Each tile can store a maximum of 256 leaf hashes. Since we only have 4 leaves for now, hashes
fit in a single file. Given it is the first tile of the tree, [its path is 00/0000/00/00/00](api/layout#tile).
Until the tile is filed with 256 leaves, the tile is "partial",
that's what the `00.04` notation means: tile `00/0000/00/00/00.04` is the partial
`00/0000/00/00/00` tile with 4 leaf hashes.

Let's look at each line of this tile file:
 - `32` that's the number of bytes used for hashes
 - `4` the number of leaf hashes in this tile
 -  the remaining lines are a series of hashes representing the node hashes of the tile: both the leaf hashes, and internal node hashes

Here is what a merkle tree with 4 leaves looks like:
```
              b
             / \
            /   \
           /     \
          a       c
         / \     / \
       h0  h1   h2  h3
       |   |    |   |
       0   1    2   3
```

In the tile file, leaves and internal node hashes are stored in the [infix tree-traversal order](https://go.dev/play/p/eZErmZdTwdB).

```bash
$ cat /tmp/mylog/tile/00/0000/00/00/00.04
32
4
hZLW82bZ0Sl/RANNZJtor87nQFCqelXHaRMLLwfsxl0= <-- h0 = sha256(0x0 + leaf_data_000)
McF1R3nScwEJFHQpESACDl9SOdg9uTRLVZaDHzLckI0= <-- a = sha256(0x1 + h0 + h1)
uHFPBFx9XQIBsGAE5pOdlEqYFgXF/PpdM1OjCEMD1K0= <-- h1 = sha256(0x0 + leaf_data_001)
DC5xrAVNktWLDv0wE9DfI1JFMx8MDoKLq2Ko/mJGDH8= <-- b = sha256(0x1 + a + c)
bLCxo8MxFM7B2UC5psSLVfssc/bvz9U67vJkRoHJtwo= <-- h2 = sha256(0x0 + leaf_data_002)
jNfnGF6uHUDupKFIaPW/QjZnPkINVKkVYc7cBakvPy4= <-- c = sha(0x1 + h2 + h3)
4Hx1iB4ewbytXkXFzD2OLIPNqBekgyRRQwkmfuMu8RU= <-- h3 = sha256(0x0 + leaf_data_003)
```

### Adding one more leaf
Let's add one more leaf to our tree.

```bash
$ echo "leaf_data_004" > $DATA_DIR/leaf_004

$ go run ./cmd/sequence --storage_dir="${LOG_DIR}" --entries "${DATA_DIR}/leaf_004"  --public_key=key.pub --origin="${LOG_ORIGIN}"
I1221 13:23:43.956356  926120 main.go:131] 4: /tmp/myfiles/leaf_004

$ go run ./cmd/integrate  --storage_dir="${LOG_DIR}"  --public_key=key.pub --private_key=key --origin="${LOG_ORIGIN}"
I1221 13:24:11.168864  926446 integrate.go:94] Loaded state with roothash 0c2e71ac054d92d58b0efd3013d0df235245331f0c0e828bab62a8fe62460c7f
I1221 13:24:11.169036  926446 integrate.go:132] New log state: size 0x5 hash: 1b26238e581181883c3f51827c58fe9c9e8a4d39383cbbabaabe0662b3c11496
```

This adds matching files in `seq`, `leaves`, and updates the checkpoint, as expected.
A new tile is availble under `00/0000/00/00/00/00.05`:

```bash
$ tree /tmp/mylog/tile
└── 00
    └── 0000
        └── 00
            └── 00
                ├── 00.04
                └── 00.05

5 directories, 2 files
```

Notice that the old tile file, `00.04` has not been deleted.

Here's the diff between the two tiles:

```bash
$ diff /tmp/mylog/tile/00/0000/00/00/00.04 /tmp/mylog/tile/00/0000/00/00/00.05
2c2
< 4
---
> 5
9a10,11
>
> 6KUzDe4gX/0rZTZCgfgBtaIGOBkOQz4duxjTT+NeM5w=
```

The number of leaves `4` has been updated to `5`, and a new leaf node hash has
appeared.  Note that even though the tree has changed shape to include this new
leaf, no internal node was added to the tile. That's because tiles only store
non-emphemeral node, and in this case, all the new internal nodes are ephemeral
(marked with a prime symbol): they will change when new leaves are added to the
tree.

``` 
                      f'
                     / \
                    /   \
                   /     \
                  /       \
                 /         \
                /           \
               /             \
              b               e'
             / \             / \
            /   \           /   \
           /     \         /     \
          a       c       d'      X
         / \     / \     / \
       h0  h1   h2  h3  h4  X
       |   |    |   |   |
       0   1    2   3   4
```

### Filling up the tile
Now, let's fill up the tile, with the maximum number of leaves it can hold: 256.

```bash
$ for i in $(seq 5 255); do x=$(printf "%03d" $i); echo "leaf_data_$x" > $DATA_DIR/leaf_$x; done;

$ go run ./cmd/sequence --storage_dir="${LOG_DIR}" --entries "${DATA_DIR}/*"  --public_key=key.pub --origin="${LOG_ORIGIN}"
I1221 13:26:19.752225  927458 main.go:131] 0: /tmp/myfiles/leaf_000 (dupe)
I1221 13:26:19.752350  927458 main.go:131] 1: /tmp/myfiles/leaf_001 (dupe)
I1221 13:26:19.752398  927458 main.go:131] 2: /tmp/myfiles/leaf_002 (dupe)
I1221 13:26:19.752442  927458 main.go:131] 3: /tmp/myfiles/leaf_003 (dupe)
I1221 13:26:19.752499  927458 main.go:131] 4: /tmp/myfiles/leaf_004 (dupe)
I1221 13:26:19.752859  927458 main.go:131] 5: /tmp/myfiles/leaf_005
I1221 13:26:19.753301  927458 main.go:131] 6: /tmp/myfiles/leaf_006
...

$ go run ./cmd/integrate  --storage_dir="${LOG_DIR}"  --public_key=key.pub --private_key=key --origin="${LOG_ORIGIN}"
I1221 13:26:22.243568  927696 integrate.go:94] Loaded state with roothash 1b26238e581181883c3f51827c58fe9c9e8a4d39383cbbabaabe0662b3c11496
I1221 13:26:22.250694  927696 integrate.go:132] New log state: size 0x100 hash: dc0d01251026e7138412adf1009ef9ed0fc55e2b9a954438b5762deb8e8519c5
```

You can check that the `seq` and `leaves` have been updated with new entries, and so has the checkpoint.

The `tile` directory now looks like this:

```bash
$ tree /tmp/mylog/tile
/tmp/mylog/tile
├── 00
│   └── 0000
│       └── 00
│           └── 00
│               ├── 00
│               ├── 00.04 -> /tmp/mylog/tile/00/0000/00/00/00
│               └── 00.05 -> /tmp/mylog/tile/00/0000/00/00/00
└── 01
    └── 0000
        └── 00
            └── 00
                └── 00.01

9 directories, 4 files
```

Since the `00/0000/00/00/00` tile is now full, its partial versions have been deleted, and now
point to the full tile.

A new tile has also appeared, one stratum above: `01/0000/00/00/00.01`. It contains a single
node, which is the current root node of the tree. To avoid storing duplicate hashes, this
top level node of the `00/0000/00/00/00` tile has been stripped, and you'll find an
empty line in this file:
```
$ cat /tmp/mylog/tile/00/0000/00/00/00
...
ZkeKg5PJFHO3e+TRuTVf4QL7tk9C9NCBkR82ipcsUxw=
iTG/pTVoZUjBJTfXcdNv2oJjxLQRKUqMOC6zVZoBznk=
R0G/vzOBrC0IdaP092TEzFn4ksrZB77kIlcAK11J7aw=

SIeXDZcyctFVLLjX3BqTs4SirwpzCezE6yZRq9OIKHw=
O876VfSKWrJ5MOQrmnO0jVgqs+vonzE/iC1t681gnAA=
YDrvejyQgwwCB0u+vwiVml4eRbc5CSaJ0rWsieOtRb4=
...
```