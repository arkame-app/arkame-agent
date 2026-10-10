//go:build race

package daemon

// comDetectorDeCorrida: com -race a memória e o tempo do processo não são os
// do agente; os tetos do teste de escala não valem.
const comDetectorDeCorrida = true
