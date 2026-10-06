// Package atualizacao troca o programa do agente por uma versão nova, sozinho
// e com volta.
//
// O painel decide quando: a resposta do heartbeat traz `update` só para o
// agente que deve atualizar agora (ver internal/daemon). Aqui fica o resto:
//
//   - baixar o programa para <pasta do programa>/arkame-agent.novo;
//   - conferir, e as três conferências são obrigatórias: a assinatura Ed25519
//     do checksums.txt contra as chaves públicas embutidas neste programa
//     (chaves.go); o SHA-256 do arquivo baixado igual ao do checksums.txt E ao
//     que o painel mandou — dois canais, quem compromete só o download ou só
//     o painel não empurra programa; e a versão estritamente maior que a atual;
//   - rodar o `self-test` do programa novo antes de pôr no lugar;
//   - trocar (o atual fica ao lado, como .anterior) e deixar o registro da
//     troca (estado.go) para o programa novo provar que funciona;
//   - a prova: cinco minutos para um heartbeat aceito pelo painel. Sem isso,
//     ou no terceiro arranque seguido sem sucesso, o .anterior volta ao lugar.
//
// No Docker o programa da imagem não pode ser trocado: o novo vai para o
// volume da configuração e o programa da imagem, ao subir, executa o do volume
// se ele for válido (lancador.go).
//
// Só biblioteca padrão: a assinatura é crypto/ed25519 e o resumo, crypto/sha256.
package atualizacao
