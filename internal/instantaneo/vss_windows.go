//go:build windows && (amd64 || arm64)

package instantaneo

// O VSS pela interface COM IVssBackupComponents, chamada direto pela tabela de
// métodos, sem cgo nem biblioteca de COM: são onze métodos, e cada índice
// abaixo é a posição dele em vsbackup.h (IUnknown ocupa 0 a 2).
//
// A sequência é a que a documentação da Microsoft dá para um requisitante de
// backup: InitializeForBackup → SetContext → SetBackupState →
// GatherWriterMetadata → StartSnapshotSet → AddToSnapshotSet →
// PrepareForBackup → DoSnapshotSet → GetSnapshotProperties. Ao soltar,
// BackupComplete e Release; o contexto de backup apaga o instantâneo no
// Release.
//
// COM pertence à thread: a criação, o uso do objeto e a liberação rodam numa
// goroutine presa à mesma thread do sistema (runtime.LockOSThread), viva
// enquanto o instantâneo existir.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	vssapi             = windows.NewLazySystemDLL("vssapi.dll")
	procCriarCompon    = vssapi.NewProc("CreateVssBackupComponentsInternal")
	procLiberarProps   = vssapi.NewProc("VssFreeSnapshotPropertiesInternal")
	ole32              = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx = ole32.NewProc("CoInitializeEx")
	procCoUninitialize = ole32.NewProc("CoUninitialize")
	procCoInitSeg      = ole32.NewProc("CoInitializeSecurity")
	oleaut32           = windows.NewLazySystemDLL("oleaut32.dll")
	procSysFreeString  = oleaut32.NewProc("SysFreeString")
)

// Suportado: este agente sabe criar o instantâneo do volume.
const Suportado = true

// Índices da tabela de métodos de IVssBackupComponents.
const (
	mRelease               = 2
	mInitializeForBackup   = 5
	mSetBackupState        = 6
	mGatherWriterMetadata  = 9
	mPrepareForBackup      = 14
	mAbortBackup           = 15
	mGatherWriterStatus    = 16
	mGetWriterStatusCount  = 17
	mFreeWriterStatus      = 18
	mGetWriterStatus       = 19
	mBackupComplete        = 27
	mSetContext            = 35
	mStartSnapshotSet      = 36
	mAddToSnapshotSet      = 37
	mDoSnapshotSet         = 38
	mGetSnapshotProperties = 42
	mIsVolumeSupported     = 44
)

// Índices de IVssAsync.
const (
	mAsyncCancel      = 3
	mAsyncQueryStatus = 5
)

const (
	coinitMultithreaded = 0x0
	rpcCAuthnPktPrivacy = 6
	rpcCImpIdentify     = 2

	vssCtxBackup = 0
	vssBtCopy    = 5

	vssSAsyncPending   = 0x00042309
	vssSAsyncFinished  = 0x0004230A
	vssSAsyncCancelled = 0x0004230B
)

// objetoCOM é um ponteiro de interface COM: a primeira palavra do objeto é a
// tabela de métodos.
type objetoCOM struct {
	vtbl *[64]uintptr
}

// chamar chama o método de índice dado.
//
// `uintptrescapes`: o ponteiro do Go convertido em uintptr NA EXPRESSÃO da
// chamada (`uintptr(unsafe.Pointer(&x))`) vai para o heap e fica vivo até ela
// voltar. Sem isso, um parâmetro de saída na pilha da goroutine podia mudar de
// lugar (a pilha cresce e é copiada) entre a conversão e o syscall, e o VSS
// escreveria na pilha velha. Por isso nenhuma chamada monta a lista de
// argumentos antes: cada conversão fica na própria chamada.
//
//go:uintptrescapes
func (o *objetoCOM) chamar(indice int, args ...uintptr) uint32 {
	todos := make([]uintptr, 0, len(args)+1)
	todos = append(todos, uintptr(unsafe.Pointer(o)))
	todos = append(todos, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[indice], todos...)
	return uint32(r)
}

func (o *objetoCOM) liberar() {
	if o != nil {
		o.chamar(mRelease)
	}
}

// propriedades é VSS_SNAPSHOT_PROP. O alinhamento do Go coincide com o do C
// em 64 bits (por isso o arquivo não compila em 386).
type propriedades struct {
	SnapshotID           windows.GUID
	SnapshotSetID        windows.GUID
	SnapshotsCount       int32
	SnapshotDeviceObject *uint16
	OriginalVolumeName   *uint16
	OriginatingMachine   *uint16
	ServiceMachine       *uint16
	ExposedName          *uint16
	ExposedPath          *uint16
	ProviderID           windows.GUID
	SnapshotAttributes   int32
	CreationTimestamp    int64
	Status               int32
}

func falhou(hr uint32) bool { return int32(hr) < 0 }

const (
	vssEObjectNotFound          = 0x80042308
	vssEVolumeNotSupported      = 0x8004230C
	vssEVolumeNotSupportedByPrv = 0x8004230E
	vssESnapshotSetInProgress   = 0x80042316
)

// semSuporte diz se o código é de volume que não aceita instantâneo: FAT32,
// exFAT, unidade de rede, `subst`.
func semSuporte(hr uint32) bool {
	return hr == vssEObjectNotFound || hr == vssEVolumeNotSupported || hr == vssEVolumeNotSupportedByPrv
}

// erroHR traduz os códigos que têm o que dizer a quem lê o painel.
func erroHR(etapa string, hr uint32) error {
	motivo := ""
	switch {
	case hr == 0x80070005:
		motivo = "acesso negado: o agente precisa rodar como administrador (o serviço roda como Sistema Local)"
	case semSuporte(hr):
		motivo = "o volume não aceita instantâneo (FAT32, exFAT, unidade de rede ou subst)"
	case hr == vssESnapshotSetInProgress:
		motivo = "outro instantâneo estava sendo criado (outro programa de backup, ou um ponto de restauração do Windows)"
	case hr == 0x80042317:
		motivo = "o volume chegou ao limite de instantâneos"
	case hr == 0x8004231F:
		motivo = "falta espaço para a área de cópia de sombra do volume"
	case hr == 0x80042306:
		motivo = "o provedor de instantâneo recusou o pedido"
	case hr == 0x80042313, hr == 0x80042314:
		motivo = "o sistema não conseguiu congelar as gravações a tempo; o disco estava ocupado demais"
	}
	if motivo != "" {
		return fmt.Errorf("%s: %s (0x%08X)", etapa, motivo, hr)
	}
	return fmt.Errorf("%s falhou (0x%08X)", etapa, hr)
}

// esperar acompanha uma operação assíncrona do VSS até o fim, ou até o
// limite, quando a cancela.
func esperar(etapa string, async *objetoCOM, limite time.Time) error {
	defer async.liberar()
	hr := new(int32)
	for {
		if r := async.chamar(mAsyncQueryStatus, uintptr(unsafe.Pointer(hr)), 0); falhou(r) {
			return erroHR(etapa, r)
		}
		switch uint32(*hr) {
		case vssSAsyncFinished:
			return nil
		case vssSAsyncCancelled:
			return fmt.Errorf("%s: cancelado", etapa)
		case vssSAsyncPending:
			if time.Now().After(limite) {
				async.chamar(mAsyncCancel)
				return fmt.Errorf("%s: não terminou no prazo", etapa)
			}
			time.Sleep(200 * time.Millisecond)
		default:
			if falhou(uint32(*hr)) {
				return erroHR(etapa, uint32(*hr))
			}
			return nil
		}
	}
}

// assincrona chama um método que devolve IVssAsync e espera por ele.
func assincrona(bc *objetoCOM, indice int, etapa string, limite time.Time) error {
	async := new(*objetoCOM)
	if r := bc.chamar(indice, uintptr(unsafe.Pointer(async))); falhou(r) {
		return erroHR(etapa, r)
	}
	return esperar(etapa, *async, limite)
}

// sessaoVSS é o instantâneo vivo.
type sessaoVSS struct {
	bc *objetoCOM
}

// encerrar avisa os escritores que o backup terminou e solta o objeto, o que
// apaga o instantâneo do contexto de backup. Rápido (o serviço parando), só
// solta: o Release apaga o instantâneo do mesmo jeito.
func (s *sessaoVSS) encerrar(rapido bool) {
	if !rapido {
		_ = assincrona(s.bc, mBackupComplete, "BackupComplete", time.Now().Add(2*time.Minute))
	}
	s.bc.liberar()
}

// montar cria o instantâneo das unidades que o aceitam e devolve o dispositivo
// de cada uma; a que não aceita (um pendrive em exFAT no plano) fica de fora
// do mapa e é lida do disco — sem derrubar o instantâneo das outras. Roda na
// thread presa; em erro, já soltou tudo.
func montar(unidades []string, limite time.Time) (s *sessaoVSS, volumes map[string]string, aviso string, err error) {
	if err := procCriarCompon.Find(); err != nil {
		return nil, nil, "", fmt.Errorf("VSS indisponível neste Windows: %w", err)
	}
	// Os escritores (SQL Server, Exchange) chamam o agente de volta; sem isto a
	// chamada de volta pode ser recusada. Vale para o processo e é refeito a
	// cada instantâneo: o CoUninitialize da última thread COM desmonta o COM,
	// e com ele a segurança definida. Já definida, RPC_E_TOO_LATE, e segue.
	menosUm := -1
	procCoInitSeg.Call(0, uintptr(menosUm), 0, 0, rpcCAuthnPktPrivacy, rpcCImpIdentify, 0, 0, 0)

	ppbc := new(*objetoCOM)
	if r, _, _ := procCriarCompon.Call(uintptr(unsafe.Pointer(ppbc))); falhou(uint32(r)) {
		return nil, nil, "", erroHR("CreateVssBackupComponents", uint32(r))
	}
	bc := *ppbc
	// Depois do StartSnapshotSet, desistir exige AbortBackup: sem ele os
	// escritores ficam presos na preparação, e a próxima tentativa dá
	// "instantâneo em andamento".
	iniciado := false
	defer func() {
		if err != nil {
			if iniciado {
				bc.chamar(mAbortBackup)
			}
			bc.liberar()
		}
	}()

	if r := bc.chamar(mInitializeForBackup, 0); falhou(r) {
		return nil, nil, "", erroHR("InitializeForBackup", r)
	}
	if r := bc.chamar(mSetContext, vssCtxBackup); falhou(r) {
		return nil, nil, "", erroHR("SetContext", r)
	}
	// Sem escolher componentes, sem estado do sistema, backup do tipo cópia,
	// sem arquivo parcial.
	if r := bc.chamar(mSetBackupState, 0, 0, vssBtCopy, 0); falhou(r) {
		return nil, nil, "", erroHR("SetBackupState", r)
	}
	if err := assincrona(bc, mGatherWriterMetadata, "GatherWriterMetadata", limite); err != nil {
		return nil, nil, "", err
	}

	provedor := new(windows.GUID) // GUID_NULL: o sistema escolhe o provedor
	nomes := map[string]*uint16{}
	var aceitas []string
	for _, u := range unidades {
		nome, err := windows.UTF16PtrFromString(u + `\`)
		if err != nil {
			return nil, nil, "", err
		}
		suportado := new(int32)
		r := isVolumeSupported(bc, provedor, nome, suportado)
		if semSuporte(r) || (!falhou(r) && *suportado == 0) {
			continue
		}
		if falhou(r) {
			return nil, nil, "", erroHR("IsVolumeSupported "+u, r)
		}
		nomes[u] = nome
		aceitas = append(aceitas, u)
	}
	if len(aceitas) == 0 {
		return nil, nil, "", errors.New("nenhum volume do plano aceita instantâneo (FAT32, exFAT, unidade de rede ou subst)")
	}

	// Outro instantâneo em criação (outro backup, um ponto de restauração do
	// Windows): espera a vez, como o restic, em vez de cair no disco vivo.
	conjunto := new(windows.GUID)
	for {
		r := bc.chamar(mStartSnapshotSet, uintptr(unsafe.Pointer(conjunto)))
		if r == vssESnapshotSetInProgress && time.Now().Add(5*time.Second).Before(limite) {
			time.Sleep(5 * time.Second)
			continue
		}
		if falhou(r) {
			return nil, nil, "", erroHR("StartSnapshotSet", r)
		}
		break
	}
	iniciado = true
	ids := map[string]*windows.GUID{}
	for _, u := range aceitas {
		id := new(windows.GUID)
		if r := addToSnapshotSet(bc, nomes[u], provedor, id); falhou(r) {
			return nil, nil, "", erroHR("AddToSnapshotSet "+u, r)
		}
		ids[u] = id
	}

	if err := assincrona(bc, mPrepareForBackup, "PrepareForBackup", limite); err != nil {
		return nil, nil, "", err
	}
	if err := assincrona(bc, mDoSnapshotSet, "DoSnapshotSet", limite); err != nil {
		return nil, nil, "", err
	}

	volumes = make(map[string]string, len(aceitas))
	for _, u := range aceitas {
		p := new(propriedades)
		if r := getSnapshotProperties(bc, ids[u], p); falhou(r) {
			return nil, nil, "", erroHR("GetSnapshotProperties "+u, r)
		}
		dispositivo := windows.UTF16PtrToString(p.SnapshotDeviceObject)
		procLiberarProps.Call(uintptr(unsafe.Pointer(p)))
		if dispositivo == "" {
			return nil, nil, "", errors.New("o instantâneo de " + u + " veio sem dispositivo")
		}
		volumes[u] = dispositivo
	}
	return &sessaoVSS{bc: bc}, volumes, escritoresComFalha(bc, limite), nil
}

// escritoresComFalha lê o estado de cada escritor depois do instantâneo. O
// DoSnapshotSet dá certo mesmo quando um escritor (o do SQL Server, o do
// Exchange) falha ao congelar: sem isto a sessão diria "vss" com os arquivos
// desse programa como depois de uma queda de energia. Melhor esforço: erro
// aqui não derruba o instantâneo, só deixa de avisar.
func escritoresComFalha(bc *objetoCOM, limite time.Time) string {
	if err := assincrona(bc, mGatherWriterStatus, "GatherWriterStatus", limite); err != nil {
		return ""
	}
	defer bc.chamar(mFreeWriterStatus)
	n := new(uint32)
	if r := bc.chamar(mGetWriterStatusCount, uintptr(unsafe.Pointer(n))); falhou(r) {
		return ""
	}
	var falhos []string
	for i := uint32(0); i < *n && i < 256; i++ {
		instancia, escritor := new(windows.GUID), new(windows.GUID)
		nome := new(*uint16) // BSTR, liberado com SysFreeString
		estado, hrFalha := new(int32), new(int32)
		r := bc.chamar(mGetWriterStatus, uintptr(i), uintptr(unsafe.Pointer(instancia)), uintptr(unsafe.Pointer(escritor)),
			uintptr(unsafe.Pointer(nome)), uintptr(unsafe.Pointer(estado)), uintptr(unsafe.Pointer(hrFalha)))
		if falhou(r) || *nome == nil {
			continue
		}
		texto := windows.UTF16PtrToString(*nome)
		procSysFreeString.Call(uintptr(unsafe.Pointer(*nome)))
		// VSS_WS_FAILED_AT_IDENTIFY (6) a VSS_WS_FAILED_AT_BACKUPSHUTDOWN (15).
		if *estado >= 6 && *estado <= 15 {
			falhos = append(falhos, texto)
		}
	}
	if len(falhos) == 0 {
		return ""
	}
	return "escritores do VSS que falharam (os arquivos deles saíram como depois de uma queda de energia): " + strings.Join(falhos, ", ")
}

// criarVSS cria o instantâneo das unidades numa thread própria, que o segura
// até a função devolvida ser chamada.
func criarVSS(ctx context.Context, unidades []string) (map[string]string, func(bool), string, error) {
	type resultado struct {
		volumes map[string]string
		aviso   string
		err     error
	}
	pronto := make(chan resultado, 1)
	soltar := make(chan bool, 1)
	terminou := make(chan struct{})
	limite, ok := ctx.Deadline()
	if !ok {
		limite = time.Now().Add(PrazoDaCriacao)
	}

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(terminou)
		r, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
		if falhou(uint32(r)) {
			pronto <- resultado{err: erroHR("CoInitializeEx", uint32(r))}
			return
		}
		defer procCoUninitialize.Call()
		s, volumes, aviso, err := montar(unidades, limite)
		pronto <- resultado{volumes, aviso, err}
		if err != nil {
			return
		}
		s.encerrar(<-soltar)
	}()

	select {
	case r := <-pronto:
		if r.err != nil {
			return nil, nil, "", r.err
		}
		fechar := func(rapido bool) {
			soltar <- rapido
			if rapido {
				return
			}
			select {
			case <-terminou:
			case <-time.After(3 * time.Minute):
			}
		}
		return r.volumes, fechar, r.aviso, nil
	case <-ctx.Done():
		// Desistimos de esperar: quando a criação terminar, solta sozinha.
		go func() {
			if r := <-pronto; r.err == nil {
				soltar <- true
			}
		}()
		return nil, nil, "", fmt.Errorf("a criação não terminou: %w", ctx.Err())
	}
}
