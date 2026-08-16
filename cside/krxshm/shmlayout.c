#include <stdio.h>
#include <stddef.h>
#include "mfsise.h"
int main(void){
  printf("MAX_FITEM=%d  MAX_FMTICK=%d\n", MAX_FITEM, MAX_FMTICK);
  printf("sizeof MFSISE_T=%zu  KBFUT_T=%zu  FSISE_T=%zu  FMAST_T=%zu  FHOGA_T=%zu  FCHEG_T=%zu  FMTICK_T=%zu\n",
    sizeof(MFSISE_T), sizeof(KBFUT_T), sizeof(FSISE_T), sizeof(FMAST_T), sizeof(FHOGA_T), sizeof(FCHEG_T), sizeof(FMTICK_T));
  printf("MFSISE_SZ(total)=%zu\n", (size_t)MFSISE_SZ);
  printf("MFSISE_T: maxN@%zu useN@%zu cDate@%zu oTime@%zu kbfut@%zu\n",
    offsetof(MFSISE_T,maxN), offsetof(MFSISE_T,useN), offsetof(MFSISE_T,cDate), offsetof(MFSISE_T,oTime), offsetof(MFSISE_T,kbfut));
  printf("KBFUT_T: futCd@%zu shrtCd@%zu baseCd@%zu ksNm@%zu fmast@%zu fsise@%zu fhoga@%zu fcheg@%zu fmtick@%zu\n",
    offsetof(KBFUT_T,futCd), offsetof(KBFUT_T,shrtCd), offsetof(KBFUT_T,baseCd), offsetof(KBFUT_T,ksNm),
    offsetof(KBFUT_T,fmast), offsetof(KBFUT_T,fsise), offsetof(KBFUT_T,fhoga), offsetof(KBFUT_T,fcheg), offsetof(KBFUT_T,fmtick));
  printf("FSISE_T: bPrc@%zu oPrc@%zu hPrc@%zu lPrc@%zu ePrc@%zu aVol@%zu aAmt@%zu yPrc@%zu diff@%zu sPrc@%zu lsPr@%zu rate@%zu sPcd@%zu sign@%zu halt@%zu\n",
    offsetof(FSISE_T,bPrc), offsetof(FSISE_T,oPrc), offsetof(FSISE_T,hPrc), offsetof(FSISE_T,lPrc), offsetof(FSISE_T,ePrc),
    offsetof(FSISE_T,aVol), offsetof(FSISE_T,aAmt), offsetof(FSISE_T,yPrc), offsetof(FSISE_T,diff),
    offsetof(FSISE_T,sPrc), offsetof(FSISE_T,lsPr), offsetof(FSISE_T,rate), offsetof(FSISE_T,sPcd), offsetof(FSISE_T,sign), offsetof(FSISE_T,halt));
  return 0;
}
