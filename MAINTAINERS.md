<!-- Generated from governance/maintainers.yaml and governance/areas.yaml by
     //governance/tools/maintainers:maintainers_gen; do not edit directly. -->

The current maintainers of the gVisor project are listed below.

Name                 | GitHub ID                                                    | Company/Organization | Areas (* = owner)
-------------------- | ------------------------------------------------------------ | -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
Fabricio Voznika     | [@fvoznika](https://github.com/fvoznika)                     | Google               | metrics, runtime_monitoring, seccomp, sentry*
Jamie Liu            | [@nixprime](https://github.com/nixprime)                     | Google               | erofs, fuse, gpu*, mm*, rdma, sentry, vfs
Zach Koopmans        | [@zkoopmans](https://github.com/zkoopmans)                   | Google               | benchmarks*, erofs, fuse, gpu, kubernetes*, libs, rdma, shim, vfs, website
Ayush Ranjan         | [@ayushr2](https://github.com/ayushr2)                       | Modal                | erofs, fuse, gpu, mm, oci, rdma*, runsc, sentry, vfs
Nayana Bidari        | [@nybidari](https://github.com/nybidari)                     | Google               | checkpointing, networking*, shim
Etienne Perot        | [@etienneperot](https://github.com/etienneperot)             | Google               | checkpointing, oci*, release_pipeline, runsc*, seccomp*, website
Lucas Manning        | [@manninglucas](https://github.com/manninglucas)             | Google               | checkpointing*, erofs*, fuse*, vfs*
Jing Chen            | [@milantracy](https://github.com/milantracy)                 | Google               | sandboxexec*, sentry, shim*, tpu
Konstantin Bogomolov | [@konstantin-s-bogom](https://github.com/konstantin-s-bogom) | Google               | mm, platform_kvm*, platform_ptrace*, platform_slimvm*, platform_systrap*, platforms*, sentry, website
Jimmy Tran           | [@trantoji](https://github.com/trantoji)                     | Google               | checkpointing, gpu, rdma, tpu*
Anil Altinay         | [@aaltinaydev](https://github.com/aaltinaydev)               | Google               | benchmarks, checkpointing, gpu, kubernetes, libs*, platform_kvm, platform_ptrace, platform_slimvm, platform_systrap, platforms, rdma, runsc, tpu
Shailend Chand       | [@shailend-g](https://github.com/shailend-g)                 | Google               | sentry
Parth Sarthi         | [@parth-opensrc](https://github.com/parth-opensrc)           | Google               | build_tooling, checklocks, gpu, libs, networking, rdma, runsc, sentry, tests
Ryan El Kochta       | [@relkochta](https://github.com/relkochta)                   | Google               | build_tooling*, checklocks*, libs, networking, release_pipeline*, tests
Rex Ren              | [@rexren-gif](https://github.com/rexren-gif)                 | Google               | build_tooling, checklocks, checkpointing, libs, metrics, networking, platform_kvm, platform_ptrace, platform_slimvm, platform_systrap, platforms, runsc, runtime_monitoring, tests
Xin Zhong            | [@xinzhong](https://github.com/xinzhong)                     | Google               | erofs, fuse, libs, metrics*, oci, runtime_monitoring*, sentry, shim, tests, vfs
Caroline Zhu         | [@carzh](https://github.com/carzh)                           | Google               | networking, runsc, sandboxexec, tests
Alexander Cueva      | [@kerumeto](https://github.com/kerumeto)                     | Google               | erofs, fuse, gpu, libs, mm, networking, platform_kvm, platform_ptrace, platform_slimvm, platform_systrap, platforms, rdma, vfs
Fernando Rivera      | [@riverafernando](https://github.com/riverafernando)         | Google               | benchmarks, kubernetes

## Specialization areas

Each area of the repository (see governance/areas.yaml for the paths) has
owners and approvers. **Owners** are accountable for the area: they curate who
may approve in it and write its standards. **Approvers** review and approve
changes in it; the reviewer assigner routes pull requests to them. Owners are
always approvers. An area marked *enforced* requires an approver's review
before merge (see CODEOWNERS): either because it is security- or
release-sensitive and its approver list is deliberately short, or because its
owners have confirmed the list and landed the area's standards.

Area               | Owners              | Approvers                                                                                         | Enforced
------------------ | ------------------- | ------------------------------------------------------------------------------------------------- | --------
benchmarks         | @zkoopmans          | @aaltinaydev @riverafernando @zkoopmans                                                           | 
build_tooling      | @relkochta          | @parth-opensrc @relkochta @rexren-gif                                                             | 
checklocks         | @relkochta          | @parth-opensrc @relkochta @rexren-gif                                                             | 
checkpointing      | @manninglucas       | @aaltinaydev @etienneperot @manninglucas @nybidari @rexren-gif @trantoji                          | 
erofs              | @manninglucas       | @ayushr2 @kerumeto @manninglucas @nixprime @xinzhong @zkoopmans                                   | 
fuse               | @manninglucas       | @ayushr2 @kerumeto @manninglucas @nixprime @xinzhong @zkoopmans                                   | 
gpu                | @nixprime           | @aaltinaydev @ayushr2 @kerumeto @nixprime @parth-opensrc @trantoji @zkoopmans                     | 
kubernetes         | @zkoopmans          | @aaltinaydev @riverafernando @zkoopmans                                                           | 
libs               | @aaltinaydev        | @aaltinaydev @kerumeto @parth-opensrc @relkochta @rexren-gif @xinzhong @zkoopmans                 | 
metrics            | @xinzhong           | @fvoznika @rexren-gif @xinzhong                                                                   | 
mm                 | @nixprime           | @ayushr2 @kerumeto @konstantin-s-bogom @nixprime                                                  | 
networking         | @nybidari           | @carzh @kerumeto @nybidari @parth-opensrc @relkochta @rexren-gif                                  | 
oci                | @etienneperot       | @ayushr2 @etienneperot @xinzhong                                                                  | 
platform_kvm       | @konstantin-s-bogom | @aaltinaydev @kerumeto @konstantin-s-bogom @rexren-gif                                            | 
platform_ptrace    | @konstantin-s-bogom | @aaltinaydev @kerumeto @konstantin-s-bogom @rexren-gif                                            | 
platform_slimvm    | @konstantin-s-bogom | @aaltinaydev @kerumeto @konstantin-s-bogom @rexren-gif                                            | 
platform_systrap   | @konstantin-s-bogom | @aaltinaydev @kerumeto @konstantin-s-bogom @rexren-gif                                            | 
platforms          | @konstantin-s-bogom | @aaltinaydev @kerumeto @konstantin-s-bogom @rexren-gif                                            | 
rdma               | @ayushr2            | @aaltinaydev @ayushr2 @kerumeto @nixprime @parth-opensrc @trantoji @zkoopmans                     | 
release_pipeline   | @relkochta          | @etienneperot @relkochta                                                                          | yes
runsc              | @etienneperot       | @aaltinaydev @ayushr2 @carzh @etienneperot @parth-opensrc @rexren-gif                             | 
runtime_monitoring | @xinzhong           | @fvoznika @rexren-gif @xinzhong                                                                   | 
sandboxexec        | @milantracy         | @carzh @milantracy                                                                                | 
seccomp            | @etienneperot       | @etienneperot @fvoznika                                                                           | yes
sentry             | @fvoznika           | @ayushr2 @fvoznika @konstantin-s-bogom @milantracy @nixprime @parth-opensrc @shailend-g @xinzhong | 
shim               | @milantracy         | @milantracy @nybidari @xinzhong @zkoopmans                                                        | 
tests              |                     | @carzh @parth-opensrc @relkochta @rexren-gif @xinzhong                                            | 
tpu                | @trantoji           | @aaltinaydev @milantracy @trantoji                                                                | 
vfs                | @manninglucas       | @ayushr2 @kerumeto @manninglucas @nixprime @xinzhong @zkoopmans                                   | 
website            |                     | @etienneperot @konstantin-s-bogom @zkoopmans                                                      | 

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
