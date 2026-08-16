/* shmlayout — MFSISE_T(파생) + MBSISE_T(채권) SHM 레이아웃 offsetof/sizeof 산출.
 * pkg/krxshm 상수의 정답지 (linux x86-64 = EC2 에서 실행해야 유효; long double 의존).
 * 빌드: cc -I cside/krxshm/inc shmlayout.c -o shmlayout */
#include <stdio.h>
#include <stddef.h>
#include "mfsise.h"
#include "mbsise.h"
int main(void){
  /* ---- 파생 (mfsise) ---- */
  printf("MAX_FITEM=%d MAX_FMTICK=%d\n", MAX_FITEM, MAX_FMTICK);
  printf("MFSISE_SZ(total)=%zu KBFUT_T=%zu FSISE_T=%zu FHOGA_T=%zu FHOGA_U=%zu\n",
    (size_t)MFSISE_SZ, sizeof(KBFUT_T), sizeof(FSISE_T), sizeof(FHOGA_T), sizeof(FHOGA_U));
  printf("MFSISE_T: maxN@%zu useN@%zu kbfut@%zu\n", offsetof(MFSISE_T,maxN), offsetof(MFSISE_T,useN), offsetof(MFSISE_T,kbfut));
  printf("KBFUT_T: futCd@%zu shrtCd@%zu fsise@%zu fhoga@%zu\n",
    offsetof(KBFUT_T,futCd), offsetof(KBFUT_T,shrtCd), offsetof(KBFUT_T,fsise), offsetof(KBFUT_T,fhoga));
  printf("FSISE_T: bPrc@%zu ePrc@%zu yPrc@%zu diff@%zu sPrc@%zu rate@%zu sign@%zu halt@%zu\n",
    offsetof(FSISE_T,bPrc), offsetof(FSISE_T,ePrc), offsetof(FSISE_T,yPrc), offsetof(FSISE_T,diff),
    offsetof(FSISE_T,sPrc), offsetof(FSISE_T,rate), offsetof(FSISE_T,sign), offsetof(FSISE_T,halt));
  printf("FHOGA_T: stVol@%zu btVol@%zu saCnt@%zu baCnt@%zu exPrc@%zu exVol@%zu shoga@%zu bhoga@%zu\n",
    offsetof(FHOGA_T,stVol), offsetof(FHOGA_T,btVol), offsetof(FHOGA_T,saCnt), offsetof(FHOGA_T,baCnt),
    offsetof(FHOGA_T,exPrc), offsetof(FHOGA_T,exVol), offsetof(FHOGA_T,shoga), offsetof(FHOGA_T,bhoga));
  printf("FHOGA_U: prc@%zu vol@%zu cnt@%zu\n", offsetof(FHOGA_U,prc), offsetof(FHOGA_U,vol), offsetof(FHOGA_U,cnt));

  /* ---- 채권 (mbsise) ---- */
  printf("MAX_BITEM=%d MAX_BMTICK=%d\n", MAX_BITEM, MAX_BMTICK);
  printf("MBSISE_SZ(total)=%zu KBOND_T=%zu BSISE_T=%zu BHOGA_T=%zu BHOGA_U=%zu\n",
    (size_t)MBSISE_SZ, sizeof(KBOND_T), sizeof(BSISE_T), sizeof(BHOGA_T), sizeof(BHOGA_U));
  printf("MBSISE_T: maxN@%zu useN@%zu bond@%zu\n", offsetof(MBSISE_T,maxN), offsetof(MBSISE_T,useN), offsetof(MBSISE_T,bond));
  printf("KBOND_T: bondCd@%zu ksNm@%zu bsise@%zu bhoga@%zu\n",
    offsetof(KBOND_T,bondCd), offsetof(KBOND_T,ksNm), offsetof(KBOND_T,bsise), offsetof(KBOND_T,bhoga));
  printf("BSISE_T: bPrc@%zu ePrc@%zu eYld@%zu yPrc@%zu yYld@%zu diff@%zu rate@%zu sign@%zu oYld@%zu hYld@%zu lYld@%zu aVol@%zu\n",
    offsetof(BSISE_T,bPrc), offsetof(BSISE_T,ePrc), offsetof(BSISE_T,eYld), offsetof(BSISE_T,yPrc), offsetof(BSISE_T,yYld),
    offsetof(BSISE_T,diff), offsetof(BSISE_T,rate), offsetof(BSISE_T,sign), offsetof(BSISE_T,oYld), offsetof(BSISE_T,hYld),
    offsetof(BSISE_T,lYld), offsetof(BSISE_T,aVol));
  printf("BHOGA_T: stVol@%zu btVol@%zu shoga@%zu bhoga@%zu  BHOGA_U: prc@%zu vol@%zu yld@%zu\n",
    offsetof(BHOGA_T,stVol), offsetof(BHOGA_T,btVol), offsetof(BHOGA_T,shoga), offsetof(BHOGA_T,bhoga),
    offsetof(BHOGA_U,prc), offsetof(BHOGA_U,vol), offsetof(BHOGA_U,yld));
  return 0;
}
