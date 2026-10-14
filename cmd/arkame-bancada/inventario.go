package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/inventario"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/cobra"
)

// cmdInventario mede o inventário do bucket contra um S3 de verdade (o RustFS
// local do laço) e, se pedido, contra um painel de verdade:
//
//	arkame-bancada inventario --endpoint http://127.0.0.1:19000 --chave K --segredo S \
//	    --bucket b --prefixo arkame/data/<agente>/ --semear 200000 --csv /tmp/versoes.csv
//	arkame-bancada inventario … --painel http://localhost:3000 --agente <id> --armazenamento <id> --token <jwt>
//	arkame-bancada inventario … --limpar
//
// --semear envia N objetos pequenos (com o metadado sha256, como o agente) e
// grava "chave<TAB>versão<TAB>tamanho<TAB>sha256" no CSV, para o catálogo do
// painel ser carregado com as versões que o bucket de fato deu.
func cmdInventario() *cobra.Command {
	var endpoint, chave, segredo, bucket, prefixo, csv, painel, agente, armazenamento, token string
	var semear int
	var limpar bool
	cmd := &cobra.Command{
		Use:   "inventario",
		Short: "Mede o inventário do bucket contra um S3 de verdade (e um painel de verdade)",
		RunE: func(*cobra.Command, []string) error {
			ctx := context.Background()
			c := s3.New(s3.Options{
				Region:                     "us-east-1",
				BaseEndpoint:               aws.String(endpoint),
				UsePathStyle:               true,
				Credentials:                credentials.NewStaticCredentialsProvider(chave, segredo, ""),
				RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
				ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
			})
			switch {
			case limpar:
				return limparPrefixo(ctx, c, bucket, prefixo)
			case semear > 0:
				return semearBucket(ctx, c, bucket, prefixo, semear, csv)
			case painel != "":
				return medirInventario(ctx, c, bucket, painel, agente, armazenamento, token)
			}
			return fmt.Errorf("diga o que fazer: --semear N, --painel URL ou --limpar")
		},
	}
	f := cmd.Flags()
	f.StringVar(&endpoint, "endpoint", "http://127.0.0.1:19000", "endpoint do S3")
	f.StringVar(&chave, "chave", "", "access key")
	f.StringVar(&segredo, "segredo", "", "secret key")
	f.StringVar(&bucket, "bucket", "", "bucket")
	f.StringVar(&prefixo, "prefixo", "", "prefixo do servidor (<prefixo>data/<agente>/)")
	f.IntVar(&semear, "semear", 0, "envia N objetos pequenos sob o prefixo")
	f.StringVar(&csv, "csv", "", "onde gravar chave, versão, tamanho e sha256 dos objetos semeados")
	f.StringVar(&painel, "painel", "", "URL do painel (roda o inventário de verdade e relata)")
	f.StringVar(&agente, "agente", "", "id do agente no painel")
	f.StringVar(&armazenamento, "armazenamento", "", "id do armazenamento no painel")
	f.StringVar(&token, "token", "", "token do agente")
	f.BoolVar(&limpar, "limpar", false, "apaga todas as versões sob o prefixo")
	return cmd
}

func semearBucket(ctx context.Context, c *s3.Client, bucket, prefixo string, n int, csv string) error {
	saida, err := os.Create(csv)
	if err != nil {
		return err
	}
	defer saida.Close()
	w := bufio.NewWriterSize(saida, 1<<20)
	defer w.Flush()
	var mu sync.Mutex
	var feitos atomic.Int64
	var primeiroErro atomic.Value
	fila := make(chan int, 256)
	var wg sync.WaitGroup
	inicio := time.Now()
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range fila {
				chave := fmt.Sprintf("%sdados/pasta-%03d/arquivo-%08d.dat", prefixo, i%997, i)
				corpo := []byte(fmt.Sprintf("conteudo %d", i))
				soma := sha256.Sum256(corpo)
				hash := hex.EncodeToString(soma[:])
				tam := int64(len(corpo))
				out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &chave, Body: bytes.NewReader(corpo), ContentLength: &tam, Metadata: map[string]string{"sha256": hash}})
				if err != nil {
					primeiroErro.CompareAndSwap(nil, err)
					continue
				}
				mu.Lock()
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", chave, aws.ToString(out.VersionId), tam, hash)
				mu.Unlock()
				feitos.Add(1)
			}
		}()
	}
	for i := range n {
		fila <- i
	}
	close(fila)
	wg.Wait()
	if err, _ := primeiroErro.Load().(error); err != nil {
		return fmt.Errorf("%d de %d enviados; primeiro erro: %w", feitos.Load(), n, err)
	}
	fmt.Printf("%d objetos enviados em %.1fs\n", feitos.Load(), time.Since(inicio).Seconds())
	return nil
}

func limparPrefixo(ctx context.Context, c *s3.Client, bucket, prefixo string) error {
	var apagados int
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: &prefixo})
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		var ids []types.ObjectIdentifier
		for _, v := range pg.Versions {
			ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, d := range pg.DeleteMarkers {
			ids = append(ids, types.ObjectIdentifier{Key: d.Key, VersionId: d.VersionId})
		}
		if len(ids) == 0 {
			continue
		}
		if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}}); err != nil {
			return err
		}
		apagados += len(ids)
	}
	fmt.Printf("%d versões apagadas de %s/%s\n", apagados, bucket, prefixo)
	return nil
}

func medirInventario(ctx context.Context, c *s3.Client, bucket, painel, agente, armazenamento, token string) error {
	cliente, err := api.New(api.Options{BaseURL: painel, Bearer: token})
	if err != nil {
		return err
	}
	cliente = cliente.ComPrazo(2 * time.Minute)
	var primeira api.InventoryPlanResponse
	inicio := time.Now()
	if err := cliente.GET(ctx, "/api/agents/"+agente+"/inventory-plan?storage_id="+url.QueryEscape(armazenamento), &primeira); err != nil {
		return fmt.Errorf("primeira página: %w", err)
	}
	if primeira.RunID == "" {
		return fmt.Errorf("o painel não emitiu inventário (agenda, capacidade ou configuração)")
	}
	var paginas int
	var noPainel time.Duration
	prazo, amostra := inventario.Limites(primeira.DeadlineSeconds, primeira.SampleSize)
	var pico atomic.Uint64
	parar := make(chan struct{})
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-parar:
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapInuse > pico.Load() {
					pico.Store(m.HeapInuse)
				}
			}
		}
	}()
	relato, _ := inventario.Rodar(ctx, inventario.Opcoes{
		Bucket:  inventario.BucketS3{Cliente: c, Nome: bucket},
		Prefixo: primeira.Prefix,
		Prazo:   prazo,
		Amostra: amostra,
		Proxima: func(ctx context.Context, cursor string) (*api.InventoryPlanResponse, error) {
			var p api.InventoryPlanResponse
			t := time.Now()
			err := cliente.GET(ctx, "/api/agents/"+agente+"/inventory-plan?run_id="+url.QueryEscape(primeira.RunID)+"&cursor="+url.QueryEscape(cursor), &p)
			noPainel += time.Since(t)
			paginas++
			return &p, err
		},
	}, &primeira)
	close(parar)
	duracao := time.Since(inicio)
	relato.RunID, relato.StartedAt, relato.FinishedAt = primeira.RunID, inicio.UTC(), time.Now().UTC()
	inventario.Caber(&relato, inventario.TetoDoCorpo)
	var resposta api.InventoryResultResponse
	if err := cliente.POST(ctx, "/api/agents/"+agente+"/inventory-result", relato, &resposta); err != nil {
		return fmt.Errorf("relato: %w", err)
	}
	fmt.Printf("completo=%v interrompido=%q versões do plano=%d encontradas=%d faltando=%d a mais=%d no bucket=%d\n",
		relato.Complete, relato.Interrupted, relato.PlanVersions, relato.Found, relato.MissingTotal, relato.ExtraVersions, relato.BucketVersions)
	fmt.Printf("tempo total %.1fs (painel: %d páginas além da primeira, %.1fs) · %d listagens · %d HEAD · amostra %d\n",
		duracao.Seconds(), paginas, noPainel.Seconds(), relato.ListRequests, relato.HeadRequests, relato.Sample.Checked)
	fmt.Printf("memória: pico residente %.0f MB · pico de heap em uso %.0f MB\n", picoResidenteMB(), float64(pico.Load())/(1<<20))
	fmt.Printf("painel: status=%s motivo=%q faltando=%d recusados=%d marcadas=%d\n", resposta.Status, resposta.Reason, resposta.Missing, resposta.Rejected, resposta.MarkedUnavailable)
	return nil
}

// picoResidenteMB é o pico de memória residente deste processo (VmHWM do
// /proc, no Linux); zero onde não há /proc.
func picoResidenteMB() float64 {
	dados, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, linha := range strings.Split(string(dados), "\n") {
		if campos := strings.Fields(linha); len(campos) >= 2 && campos[0] == "VmHWM:" {
			kb, _ := strconv.ParseFloat(campos[1], 64)
			return kb / 1024
		}
	}
	return 0
}
