<!-- Generated from governance/maintainers.yaml and governance/areas.yaml by
     //governance/tools/maintainers:maintainers_gen; do not edit directly. -->

The current maintainers of the gVisor project are listed below.

Name                 | GitHub ID                                                    | Company/Organization | Areas (* = owner)
-------------------- | ------------------------------------------------------------ | -------------------- | --------------------------------------------------------------------------------------------------------------------
Fabricio Voznika     | [@fvoznika](https://github.com/fvoznika)                     | Google               | observability, seccomp
Jamie Liu            | [@nixprime](https://github.com/nixprime)                     | Google               | fuse, gofer, mm, sentry, vfs
Zach Koopmans        | [@zkoopmans](https://github.com/zkoopmans)                   | Google               | benchmarks*, fuse, kubernetes*, shim
Ayush Ranjan         | [@ayushr2](https://github.com/ayushr2)                       | Modal                | fuse, gofer, gpu, mm, sentry, vfs
Nayana Bidari        | [@nybidari](https://github.com/nybidari)                     | Google               | checkpointing, networking*, shim
Etienne Perot        | [@etienneperot](https://github.com/etienneperot)             | Google               | benchmarks, checkpointing, kubernetes, observability, release_pipeline, runsc*, sandboxexec, seccomp*, shim, website
Lucas Manning        | [@manninglucas](https://github.com/manninglucas)             | Google               | checkpointing*, fuse*, gofer, runsc, sandboxexec*, sentry, tpu*, vfs
Jing Chen            | [@milantracy](https://github.com/milantracy)                 | Google               | sandboxexec, shim*, tpu
Konstantin Bogomolov | [@konstantin-s-bogom](https://github.com/konstantin-s-bogom) | Google               | platforms*
Jimmy Tran           | [@trantoji](https://github.com/trantoji)                     | Google               | checkpointing, tpu
Anil Altinay         | [@aaltinaydev](https://github.com/aaltinaydev)               | Google               | benchmarks, checkpointing, kubernetes, platforms, runsc, tpu
Shailend Chand       | [@shailend-g](https://github.com/shailend-g)                 | Google               | 
Parth Sarthi         | [@parth-opensrc](https://github.com/parth-opensrc)           | Google               | build_tooling, gpu, networking, runsc
Ryan El Kochta       | [@relkochta](https://github.com/relkochta)                   | Google               | build_tooling*, networking, release_pipeline*
Rex Ren              | [@rexren-gif](https://github.com/rexren-gif)                 | Google               | build_tooling, checkpointing, networking, observability, runsc
Xin Zhong            | [@xinzhong](https://github.com/xinzhong)                     | Google               | fuse, observability*, runsc, shim
Caroline Zhu         | [@carzh](https://github.com/carzh)                           | Google               | networking, runsc, sandboxexec
Alexander Cueva      | [@kerumeto](https://github.com/kerumeto)                     | Google               | networking, platforms
Fernando Rivera      | [@riverafernando](https://github.com/riverafernando)         | Google               | benchmarks, checkpointing, kubernetes

## Specialization areas

Each area of the repository (see governance/areas.yaml for the paths) has
owners and approvers. **Owners** are accountable for the area: they curate who
may approve in it and write its standards. **Approvers** review and approve
changes in it; the reviewer assigner routes pull requests to them. Owners are
always approvers. An area marked *enforced* requires an approver's review
before merge (see CODEOWNERS): either because it is security- or
release-sensitive and its approver list is deliberately short, or because its
owners have confirmed the list and landed the area's standards.

Area             | Owners              | Approvers                                                                                | Enforced
---------------- | ------------------- | ---------------------------------------------------------------------------------------- | --------
benchmarks       | @zkoopmans          | @aaltinaydev @etienneperot @riverafernando @zkoopmans                                    | 
build_tooling    | @relkochta          | @parth-opensrc @relkochta @rexren-gif                                                    | 
checkpointing    | @manninglucas       | @aaltinaydev @etienneperot @manninglucas @nybidari @rexren-gif @riverafernando @trantoji | 
fuse             | @manninglucas       | @ayushr2 @manninglucas @nixprime @xinzhong @zkoopmans                                    | 
gofer            |                     | @ayushr2 @manninglucas @nixprime                                                         | 
gpu              |                     | @ayushr2 @parth-opensrc                                                                  | 
kubernetes       | @zkoopmans          | @aaltinaydev @etienneperot @riverafernando @zkoopmans                                    | 
libs             |                     |                                                                                          | 
mm               |                     | @ayushr2 @nixprime                                                                       | 
networking       | @nybidari           | @carzh @kerumeto @nybidari @parth-opensrc @relkochta @rexren-gif                         | 
observability    | @xinzhong           | @etienneperot @fvoznika @rexren-gif @xinzhong                                            | 
platforms        | @konstantin-s-bogom | @aaltinaydev @kerumeto @konstantin-s-bogom                                               | 
release_pipeline | @relkochta          | @etienneperot @relkochta                                                                 | yes
runsc            | @etienneperot       | @aaltinaydev @carzh @etienneperot @manninglucas @parth-opensrc @rexren-gif @xinzhong     | 
sandboxexec      | @manninglucas       | @carzh @etienneperot @manninglucas @milantracy                                           | 
seccomp          | @etienneperot       | @etienneperot @fvoznika                                                                  | yes
sentry           |                     | @ayushr2 @manninglucas @nixprime                                                         | 
shim             | @milantracy         | @etienneperot @milantracy @nybidari @xinzhong @zkoopmans                                 | 
tests            |                     |                                                                                          | 
tpu              | @manninglucas       | @aaltinaydev @manninglucas @milantracy @trantoji                                         | 
vfs              |                     | @ayushr2 @manninglucas @nixprime                                                         | 
website          |                     | @etienneperot                                                                            | 

## Emeritus Maintainers

Maintainers who have stepped back from active participation are recognized here.
Emeritus maintainers do not have voting rights or merge access but are
recognized for their contributions and may be consulted on project matters.

A maintainer is moved to emeritus status after 12 months of inactivity, or by
request. An emeritus maintainer may return to active status through the normal
maintainer nomination process.

Name                 | GitHub ID                                            | Date moved to emeritus
-------------------- | ---------------------------------------------------- | ----------------------
Adin Scannell        | [@amscanne](https://github.com/amscanne)             | 2023-03-03
Nicolas Lacasse      | [@nlacasse](https://github.com/nlacasse)             | 2025-06-27
Bhasker Hariharan    | [@hbhasker](https://github.com/hbhasker)             | 2022-09-16
Ian Gudger           | [@iangudger](https://github.com/iangudger)           | 2020-06-26
Michael Pratt        | [@prattmic](https://github.com/prattmic)             | 2020-04-02
Kevin Krakauer       | [@kevinGC](https://github.com/kevinGC)               | 2025-01-28
Rahat Mahmood        | [@mrahatm](https://github.com/mrahatm)               | 2023-01-19
Andrei Vagin         | [@avagin](https://github.com/avagin)                 | 2026-02-12
Tamir Duberstein     | [@tamird](https://github.com/tamird)                 | 2022-07-12
Ian Lewis            | [@ianlewis](https://github.com/ianlewis)             | 2023-07-17
Ghanan Gowripalan    | [@ghananigans](https://github.com/ghananigans)       | 2023-10-16
Shambhavi Srivastava | [@sshambhavi-252](https://github.com/sshambhavi-252) | 2023-10-10
